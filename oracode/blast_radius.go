package oracode

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

type BlastRadiusNode struct {
	Symbol      string             `json:"symbol"`
	File        string             `json:"file,omitempty"`
	Line        int                `json:"line,omitempty"`
	Kind        string             `json:"kind,omitempty"`
	Callers     []string           `json:"callers,omitempty"`
	Routes      []string           `json:"routes,omitempty"`
	Frontend    []string           `json:"frontend,omitempty"`
	Tables      []string           `json:"tables,omitempty"`
	Children    []*BlastRadiusNode `json:"children,omitempty"`
	Confidence  string             `json:"confidence,omitempty"`
	Framework   string             `json:"framework,omitempty"`
	Middlewares []string           `json:"middlewares,omitempty"`
}

func (s *MCPServer) ReverseChain(symbol string, maxDepth int) (*BlastRadiusNode, error) {
	if maxDepth < 1 {
		maxDepth = 1
	}
	defs := s.idx.FindDefinitions(symbol)
	if len(defs) == 0 {
		return nil, fmt.Errorf("symbol %q not found", symbol)
	}
	root := &BlastRadiusNode{
		Symbol: symbol,
		File:   defs[0].File,
		Line:   defs[0].Line,
		Kind:   defs[0].Kind,
	}
	visited := map[string]bool{}
	return s.reverseWalk(symbol, maxDepth, 0, visited, root)
}

func (s *MCPServer) reverseWalk(symbol string, maxDepth, depth int, visited map[string]bool, node *BlastRadiusNode) (*BlastRadiusNode, error) {
	if node == nil {
		node = &BlastRadiusNode{Symbol: symbol}
	}
	if depth > maxDepth || visited[symbol] {
		return node, nil
	}
	visited[symbol] = true

	defs := s.idx.FindDefinitions(symbol)
	if len(defs) > 0 {
		node.File = defs[0].File
		node.Line = defs[0].Line
		node.Kind = defs[0].Kind
	}

	node.Callers = uniqueStrings(append(node.Callers, callersForSymbol(s, symbol)...))
	node.Routes = uniqueStrings(append(node.Routes, routesForHandler(s, symbol)...))
	node.Frontend = uniqueStrings(append(node.Frontend, frontendCallsForSymbol(s, symbol)...))
	node.Tables = uniqueStrings(append(node.Tables, tablesForSymbol(s, symbol)...))

	if depth >= maxDepth {
		return node, nil
	}

	callers := callersForSymbol(s, symbol)
	sort.Strings(callers)
	for _, caller := range callers {
		if visited[caller] {
			continue
		}
		child := &BlastRadiusNode{Symbol: caller}
		child, _ = s.reverseWalk(caller, maxDepth, depth+1, visited, child)
		if child != nil {
			node.Children = append(node.Children, child)
		}
	}
	return node, nil
}

func callersForSymbol(s *MCPServer, symbol string) []string {
	refs := s.findRefs(symbol)
	callers := make(map[string]struct{})
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		caller := s.findEnclosingFunction(ref.File, ref.Line)
		if caller == "" || caller == symbol {
			continue
		}
		callers[caller] = struct{}{}
	}
	out := make([]string, 0, len(callers))
	for caller := range callers {
		out = append(out, caller)
	}
	sort.Strings(out)
	return out
}

func routesForHandler(s *MCPServer, symbol string) []string {
	routes, err := s.loadRouteEntries("", 10000)
	if err != nil {
		return nil
	}
	var out []string
	for _, route := range routes {
		handler := firstNonEmpty(route.Resolved, route.Handler)
		if handler == "" {
			continue
		}
		if handler == symbol || strings.HasSuffix(handler, "."+symbol) || strings.HasSuffix(handler, symbol) {
			out = append(out, fmt.Sprintf("%s %s (%s:%d)", route.Method, route.Path, route.File, route.Line))
		}
	}
	return uniqueStrings(out)
}

func frontendCallsForSymbol(s *MCPServer, symbol string) []string {
	graph, err := LoadFrontendAPIIndex(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "frontend_api.json"))
	if err != nil || graph == nil {
		return nil
	}
	routePaths := routesForHandler(s, symbol)
	if len(routePaths) == 0 {
		return nil
	}
	var out []string
	for _, call := range graph.Calls {
		for _, routeLine := range routePaths {
			routePath := extractRoutePath(routeLine)
			if routePath != "" && (call.URL == routePath || strings.HasSuffix(call.URL, routePath) || strings.HasSuffix(routePath, call.URL)) {
				out = append(out, fmt.Sprintf("%s:%d %s %s", call.Component, call.Line, call.Method, call.URL))
			}
		}
	}
	return uniqueStrings(out)
}

func extractRoutePath(routeLine string) string {
	parts := strings.Split(routeLine, " ")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func tablesForSymbol(s *MCPServer, symbol string) []string {
	defs := s.idx.FindDefinitions(symbol)
	if len(defs) == 0 {
		return nil
	}
	def := defs[0]
	absPath, err := s.idx.Policy.ResolveWorkspacePath(def.File)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, data, parser.ParseComments)
	if err != nil {
		return nil
	}
	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if candidate, ok := n.(*ast.FuncDecl); ok && candidate.Name != nil && candidate.Name.Name == symbol {
			fn = candidate
			return false
		}
		return true
	})
	if fn == nil {
		return nil
	}
	tableNames := loadSQLTableNames(s.idx.Policy.WorkspaceRoot)
	return collectTablesFromBody(fn.Body, tableNames)
}

func formatBlastTree(node *BlastRadiusNode, indent string) string {
	if node == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s- %s", indent, node.Symbol)
	if node.File != "" {
		fmt.Fprintf(&out, " (%s:%d)", node.File, node.Line)
	}
	out.WriteString("\n")
	if len(node.Routes) > 0 {
		fmt.Fprintf(&out, "%s  routes: %s\n", indent, strings.Join(node.Routes, ", "))
	}
	if len(node.Frontend) > 0 {
		fmt.Fprintf(&out, "%s  frontend: %s\n", indent, strings.Join(node.Frontend, ", "))
	}
	if len(node.Tables) > 0 {
		fmt.Fprintf(&out, "%s  tables: %s\n", indent, strings.Join(node.Tables, ", "))
	}
	if len(node.Callers) > 0 {
		fmt.Fprintf(&out, "%s  callers: %s\n", indent, strings.Join(node.Callers, ", "))
	}
	for _, child := range node.Children {
		out.WriteString(formatBlastTree(child, indent+"  "))
	}
	return out.String()
}
