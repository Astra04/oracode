package oracode

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const routeGraphFileName = "route_graph.json"

type CallEdge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	File   string `json:"file"`
	Line   int    `json:"line"`
}

type RouteNode struct {
	Method      string     `json:"method"`
	Path        string     `json:"path"`
	Handler     string     `json:"handler,omitempty"`
	Resolved    string     `json:"resolved_handler,omitempty"`
	File        string     `json:"file"`
	Line        int        `json:"line"`
	Receiver    string     `json:"receiver,omitempty"`
	Middlewares []string   `json:"middlewares,omitempty"`
	Framework   string     `json:"framework,omitempty"`
	Confidence  string     `json:"confidence,omitempty"` // exact|inferred
	CallGraph   []CallEdge `json:"call_graph,omitempty"`
}

type RouteGraph struct {
	GeneratedAt string      `json:"generated_at"`
	Routes      []RouteNode `json:"routes"`
}

func (idx *Index) RefreshRouteGraph() error {
	rg, err := idx.BuildRouteGraph()
	if err != nil {
		return err
	}
	root := idx.Policy.WorkspaceRoot
	oracodeDir := workspaceStateRoot(root)
	if err := os.MkdirAll(oracodeDir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	outPath := workspaceStatePath(root, routeGraphFileName)
	data, err := json.MarshalIndent(rg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal route graph: %w", err)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return fmt.Errorf("write route graph: %w", err)
	}
	return nil
}

func (idx *Index) LoadRouteGraph() (*RouteGraph, error) {
	path := workspaceStatePath(idx.Policy.WorkspaceRoot, routeGraphFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rg RouteGraph
	if err := json.Unmarshal(data, &rg); err != nil {
		return nil, fmt.Errorf("decode route graph: %w", err)
	}
	return &rg, nil
}

func (idx *Index) BuildRouteGraph() (*RouteGraph, error) {
	files := idx.candidateGoFiles()
	nodes := make([]RouteNode, 0, 512)
	allEdges := make([]CallEdge, 0)

	for _, rel := range files {
		absPath, err := idx.Policy.ResolveWorkspacePath(rel)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		edges, err := extractCallGraph(rel, data)
		if err == nil {
			allEdges = append(allEdges, edges...)
		}
		nodes = append(nodes, extractRouteNodes(rel, data)...)
	}

	// Build caller -> callees map for each node's handler
	callerMap := make(map[string][]CallEdge)
	for _, e := range allEdges {
		callerMap[e.Caller] = append(callerMap[e.Caller], e)
	}

	idx.enrichRouteHandlerResolution(nodes)

	// Attach call chains to route handlers (up to depth 5)
	for i, node := range nodes {
		rootFunc := node.Resolved
		if rootFunc == "" {
			rootFunc = node.Handler
		}
		if rootFunc != "" {
			chain := buildCallChain(rootFunc, callerMap, 5)
			nodes[i].CallGraph = chain
		}
	}

	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].File == nodes[j].File {
			if nodes[i].Line == nodes[j].Line {
				if nodes[i].Path == nodes[j].Path {
					return nodes[i].Method < nodes[j].Method
				}
				return nodes[i].Path < nodes[j].Path
			}
			return nodes[i].Line < nodes[j].Line
		}
		return nodes[i].File < nodes[j].File
	})
	return &RouteGraph{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Routes:      nodes,
	}, nil
}

func (idx *Index) enrichRouteHandlerResolution(nodes []RouteNode) {
	for i := range nodes {
		handler := strings.TrimSpace(nodes[i].Handler)
		if handler == "" {
			continue
		}
		resolved := resolveHandlerSymbol(handler)
		if resolved == "" {
			continue
		}
		defs := idx.FindDefinitions(resolved)
		if len(defs) == 0 {
			continue
		}
		nodes[i].Resolved = defs[0].Name
		nodes[i].Confidence = "exact"
	}
}

func (idx *Index) candidateGoFiles() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]string, 0, len(idx.Files))
	for rel, meta := range idx.Files {
		if meta != nil && meta.Language == LanguageGo {
			out = append(out, filepath.ToSlash(filepath.Clean(rel)))
		}
	}
	sort.Strings(out)
	return out
}

func extractRouteNodes(rel string, source []byte) []RouteNode {
	framework := detectFramework(source)
	return extractRouteNodesGeneric(rel, source, framework)
}

func extractRouteNodesGeneric(rel string, source []byte, framework string) []RouteNode {
	lines := strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
	groupPrefix := map[string]string{}
	groupMiddlewares := map[string][]string{}
	withMiddlewares := map[string][]string{}

	groupAssign := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*([A-Za-z_][A-Za-z0-9_\.]*)\.Group\(\s*"([^"]*)"\s*\)`)
	routeAssign := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*([A-Za-z_][A-Za-z0-9_\.]*)\.(Route|Mount)\(\s*"([^"]*)"\s*,?`)
	withAssign := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*([A-Za-z_][A-Za-z0-9_\.]*)\.With\(([^)]*)\)`)
	useCall := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_\.]*)\.Use\(([^)]*)\)`)
	methodCall := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_\.]*)\.(GET|POST|PUT|PATCH|DELETE|OPTIONS|HEAD)\(\s*"([^"]+)"\s*,\s*([^)]+)\)`)
	methodCallLower := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_\.]*)\.(Get|Post|Put|Patch|Delete|Options|Head|Any)\(\s*"([^"]+)"\s*,\s*([^)]+)\)`)
	handleCall := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_\.]*)\.(Handle|HandleFunc)\(\s*"([^"]+)"\s*,\s*([^)]+)\)`)

	var nodes []RouteNode
	for i, line := range lines {
		if m := groupAssign.FindStringSubmatch(line); len(m) > 0 {
			child := m[1]
			parent := baseReceiver(m[2])
			frag := m[3]
			groupPrefix[child] = joinRoutePath(groupPrefix[parent], frag)
			groupMiddlewares[child] = append([]string{}, groupMiddlewares[parent]...)
			continue
		}
		if m := routeAssign.FindStringSubmatch(line); len(m) > 0 {
			child := m[1]
			parent := baseReceiver(m[2])
			frag := m[4]
			groupPrefix[child] = joinRoutePath(groupPrefix[parent], frag)
			groupMiddlewares[child] = append([]string{}, groupMiddlewares[parent]...)
			continue
		}
		if m := withAssign.FindStringSubmatch(line); len(m) > 0 {
			child := m[1]
			parent := baseReceiver(m[2])
			groupPrefix[child] = groupPrefix[parent]
			groupMiddlewares[child] = append([]string{}, groupMiddlewares[parent]...)
			withMiddlewares[child] = append(withMiddlewares[child], parseArgList(m[3])...)
			withMiddlewares[child] = uniqueStrings(withMiddlewares[child])
			continue
		}
		if m := useCall.FindStringSubmatch(line); len(m) > 0 {
			receiver := baseReceiver(m[1])
			groupMiddlewares[receiver] = append(groupMiddlewares[receiver], parseArgList(m[2])...)
			groupMiddlewares[receiver] = uniqueStrings(groupMiddlewares[receiver])
			continue
		}

		if m := methodCall.FindStringSubmatch(line); len(m) > 0 {
			receiver := baseReceiver(m[1])
			if !strings.HasPrefix(m[3], "/") {
				continue
			}
			mws := append([]string{}, groupMiddlewares[receiver]...)
			mws = append(mws, withMiddlewares[receiver]...)
			mws = uniqueStrings(mws)
			nodes = append(nodes, RouteNode{
				Method:      strings.ToUpper(m[2]),
				Path:        joinRoutePath(groupPrefix[receiver], m[3]),
				Handler:     cleanHandlerExpr(m[4]),
				File:        rel,
				Line:        i + 1,
				Receiver:    receiver,
				Middlewares: mws,
				Framework:   framework,
				Confidence:  "inferred",
			})
			continue
		}
		if m := methodCallLower.FindStringSubmatch(line); len(m) > 0 {
			receiver := baseReceiver(m[1])
			if !strings.HasPrefix(m[3], "/") {
				continue
			}
			mws := append([]string{}, groupMiddlewares[receiver]...)
			mws = append(mws, withMiddlewares[receiver]...)
			mws = uniqueStrings(mws)
			nodes = append(nodes, RouteNode{
				Method:      strings.ToUpper(m[2]),
				Path:        joinRoutePath(groupPrefix[receiver], m[3]),
				Handler:     cleanHandlerExpr(m[4]),
				File:        rel,
				Line:        i + 1,
				Receiver:    receiver,
				Middlewares: mws,
				Framework:   framework,
				Confidence:  "inferred",
			})
			continue
		}
		if m := handleCall.FindStringSubmatch(line); len(m) > 0 {
			receiver := baseReceiver(m[1])
			if !strings.HasPrefix(m[3], "/") {
				continue
			}
			mws := append([]string{}, groupMiddlewares[receiver]...)
			mws = append(mws, withMiddlewares[receiver]...)
			mws = uniqueStrings(mws)
			nodes = append(nodes, RouteNode{
				Method:      "ANY",
				Path:        joinRoutePath(groupPrefix[receiver], m[3]),
				Handler:     cleanHandlerExpr(m[4]),
				File:        rel,
				Line:        i + 1,
				Receiver:    receiver,
				Middlewares: mws,
				Framework:   framework,
				Confidence:  "inferred",
			})
		}
	}
	return nodes
}

func baseReceiver(expr string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return ""
	}
	if i := strings.Index(expr, "."); i >= 0 {
		return strings.TrimSpace(expr[:i])
	}
	return expr
}

func parseArgList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func cleanHandlerExpr(raw string) string {
	h := strings.TrimSpace(raw)
	if idx := strings.Index(h, ","); idx >= 0 {
		h = strings.TrimSpace(h[:idx])
	}
	return h
}

func resolveHandlerSymbol(handlerExpr string) string {
	h := strings.TrimSpace(handlerExpr)
	if h == "" {
		return ""
	}
	if i := strings.Index(h, "("); i > 0 {
		return strings.TrimSpace(h[:i])
	}
	return h
}

func detectFramework(source []byte) string {
	text := string(source)
	switch {
	case strings.Contains(text, `"github.com/gin-gonic/gin"`):
		return "gin"
	case strings.Contains(text, `"github.com/go-chi/chi"`):
		return "chi"
	case strings.Contains(text, `"github.com/labstack/echo"`):
		return "echo"
	default:
		return "generic"
	}
}

func joinRoutePath(prefix, frag string) string {
	p := strings.TrimSpace(prefix)
	f := strings.TrimSpace(frag)
	if p == "" {
		if strings.HasPrefix(f, "/") {
			return f
		}
		return "/" + strings.TrimPrefix(f, "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + strings.TrimPrefix(p, "/")
	}
	if f == "" {
		return p
	}
	if strings.HasPrefix(f, "/") {
		return strings.TrimRight(p, "/") + f
	}
	return strings.TrimRight(p, "/") + "/" + f
}

// extractCallGraph parses a Go file and returns a list of CallEdges.
func extractCallGraph(file string, src []byte) ([]CallEdge, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return nil, err
	}
	var edges []CallEdge
	var currentFunc string

	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		switch fd := n.(type) {
		case *ast.FuncDecl:
			prevFunc := currentFunc
			currentFunc = fd.Name.Name
			if fd.Body != nil {
				ast.Inspect(fd.Body, func(child ast.Node) bool {
					if child == nil {
						return true
					}
					if call, ok := child.(*ast.CallExpr); ok {
						var callee string
						switch fun := call.Fun.(type) {
						case *ast.Ident:
							callee = fun.Name
						case *ast.SelectorExpr:
							callee = fun.Sel.Name
						}
						if callee != "" {
							edges = append(edges, CallEdge{
								Caller: currentFunc,
								Callee: callee,
								File:   file,
								Line:   fset.Position(call.Pos()).Line,
							})
						}
					}
					return true
				})
			}
			currentFunc = prevFunc
			return false
		}
		return true
	})
	return edges, nil
}

// buildCallChain recursively collects call edges up to depth.
func buildCallChain(start string, callerMap map[string][]CallEdge, maxDepth int) []CallEdge {
	visited := make(map[string]bool)
	var result []CallEdge
	var dfs func(symbol string, depth int)
	dfs = func(symbol string, depth int) {
		if depth > maxDepth || visited[symbol] {
			return
		}
		visited[symbol] = true
		for _, edge := range callerMap[symbol] {
			result = append(result, edge)
			dfs(edge.Callee, depth+1)
		}
		visited[symbol] = false
	}
	dfs(start, 0)
	return result
}
