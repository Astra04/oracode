package oracode

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type ChainTraceNode struct {
	Symbol      string   `json:"symbol"`
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Calls       []string `json:"calls,omitempty"`
	Tables      []string `json:"tables,omitempty"`
	Confidence  string   `json:"confidence,omitempty"`
	Framework   string   `json:"framework,omitempty"`
	Middlewares []string `json:"middlewares,omitempty"`
}

type ChainTraceReport struct {
	Method string           `json:"method"`
	URL    string           `json:"url"`
	Route  *routeEntry      `json:"route,omitempty"`
	Nodes  []ChainTraceNode `json:"nodes,omitempty"`
	Tables []string         `json:"tables,omitempty"`
	Notes  []string         `json:"notes,omitempty"`
}

func (s *MCPServer) traceChain(urlPath, method string, depth int) (string, error) {
	routes, err := s.loadRouteEntries("", 10000)
	if err != nil {
		return "", err
	}
	route := findRouteMatch(routes, urlPath, method)
	if route == nil {
		return "", fmt.Errorf("no route match found for %s %s", methodOrAny(method), urlPath)
	}

	start := route.Handler
	if route.Resolved != "" {
		start = route.Resolved
	}
	start = resolveHandlerSymbol(start)
	if start == "" {
		return "", fmt.Errorf("route %s has no traceable handler", route.Path)
	}

	report := ChainTraceReport{
		Method: strings.ToUpper(strings.TrimSpace(method)),
		URL:    urlPath,
		Route:  route,
	}

	visited := make(map[string]bool)
	tablesSeen := make(map[string]bool)
	nodes, tables, notes := s.traceGoSymbol(start, depth, visited, tablesSeen)
	report.Nodes = nodes
	report.Tables = tables
	report.Notes = notes

	var out strings.Builder
	fmt.Fprintf(&out, "## Trace Chain %s %s\n", methodOrAny(method), urlPath)
	fmt.Fprintf(&out, "- route: [%s] %s (%s:%d)\n", route.Method, route.Path, route.File, route.Line)
	if route.Handler != "" {
		fmt.Fprintf(&out, "  handler: %s\n", route.Handler)
	}
	if route.Resolved != "" && route.Resolved != route.Handler {
		fmt.Fprintf(&out, "  resolved: %s\n", route.Resolved)
	}
	if route.Framework != "" {
		fmt.Fprintf(&out, "  framework: %s\n", route.Framework)
	}
	if route.Confidence != "" {
		fmt.Fprintf(&out, "  confidence: %s\n", route.Confidence)
	}
	if len(route.Middlewares) > 0 {
		fmt.Fprintf(&out, "  middleware: %s\n", strings.Join(route.Middlewares, " -> "))
	}
	if len(nodes) == 0 {
		out.WriteString("\nNo downstream Go calls traced.\n")
	} else {
		out.WriteString("\n## Call Chain\n")
		for i, node := range nodes {
			indent := strings.Repeat("  ", i)
			fmt.Fprintf(&out, "%s- %s (%s:%d)\n", indent, node.Symbol, node.File, node.Line)
			if len(node.Calls) > 0 {
				fmt.Fprintf(&out, "%s  calls: %s\n", indent, strings.Join(node.Calls, ", "))
			}
			if len(node.Tables) > 0 {
				fmt.Fprintf(&out, "%s  tables: %s\n", indent, strings.Join(node.Tables, ", "))
			}
		}
	}
	if len(tables) > 0 {
		out.WriteString("\n## SQL Tables Reached\n")
		for _, t := range tables {
			fmt.Fprintf(&out, "- %s\n", t)
		}
	}
	if len(notes) > 0 {
		out.WriteString("\n## Notes\n")
		for _, note := range notes {
			fmt.Fprintf(&out, "- %s\n", note)
		}
	}
	return out.String(), nil
}

func findRouteMatch(routes []routeEntry, urlPath, method string) *routeEntry {
	method = strings.ToUpper(strings.TrimSpace(method))
	urlPath = strings.TrimSpace(urlPath)
	for i := range routes {
		r := &routes[i]
		if r.Path != urlPath {
			continue
		}
		if method != "" && r.Method != "ANY" && r.Method != method {
			continue
		}
		return r
	}
	for i := range routes {
		r := &routes[i]
		if strings.HasSuffix(r.Path, urlPath) {
			if method == "" || r.Method == "ANY" || r.Method == method {
				return r
			}
		}
	}
	return nil
}

func (s *MCPServer) traceGoSymbol(name string, depth int, visited map[string]bool, tablesSeen map[string]bool) ([]ChainTraceNode, []string, []string) {
	if depth < 1 {
		depth = 1
	}
	if visited[name] {
		return nil, nil, nil
	}
	visited[name] = true

	defs := s.idx.FindDefinitions(name)
	if len(defs) == 0 {
		return []ChainTraceNode{{Symbol: name}}, nil, []string{fmt.Sprintf("no Go definition found for %q", name)}
	}

	def := defs[0]
	absPath, err := s.idx.Policy.ResolveWorkspacePath(def.File)
	if err != nil {
		return []ChainTraceNode{{Symbol: name, File: def.File, Line: def.Line}}, nil, []string{err.Error()}
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return []ChainTraceNode{{Symbol: name, File: def.File, Line: def.Line}}, nil, []string{err.Error()}
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, absPath, src, parser.ParseComments)
	if err != nil {
		return []ChainTraceNode{{Symbol: name, File: def.File, Line: def.Line}}, nil, []string{err.Error()}
	}

	tableNames := loadSQLTableNames(s.idx.Policy.WorkspaceRoot)
	var fn *ast.FuncDecl
	ast.Inspect(parsed, func(n ast.Node) bool {
		if candidate, ok := n.(*ast.FuncDecl); ok && candidate.Name != nil && candidate.Name.Name == name {
			fn = candidate
			return false
		}
		return true
	})
	if fn == nil {
		return []ChainTraceNode{{Symbol: name, File: def.File, Line: def.Line}}, nil, []string{fmt.Sprintf("function %q not found in parsed AST", name)}
	}

	callNames := collectGoCalls(fn.Body)
	node := ChainTraceNode{
		Symbol:      name,
		File:        def.File,
		Line:        def.Line,
		Calls:       callNames,
		Tables:      collectTablesFromBody(fn.Body, tableNames),
		Confidence:  "inferred",
		Framework:   routeFrameworkForFile(def.File),
		Middlewares: nil,
	}
	for _, t := range node.Tables {
		tablesSeen[t] = true
	}

	var children []ChainTraceNode
	var allNotes []string
	if depth > 1 {
		for _, call := range callNames {
			if visited[call] {
				continue
			}
			if len(s.idx.FindDefinitions(call)) == 0 {
				continue
			}
			childNodes, _, childNotes := s.traceGoSymbol(call, depth-1, visited, tablesSeen)
			if len(childNodes) > 0 {
				children = append(children, childNodes...)
			}
			allNotes = append(allNotes, childNotes...)
		}
	}

	uniqTables := make([]string, 0, len(tablesSeen))
	for t := range tablesSeen {
		uniqTables = append(uniqTables, t)
	}
	sort.Strings(uniqTables)
	return append([]ChainTraceNode{node}, children...), uniqTables, allNotes
}

func collectGoCalls(body *ast.BlockStmt) []string {
	if body == nil {
		return nil
	}
	seen := map[string]bool{}
	var calls []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name := fun.Name
			if shouldSkipCall(name) {
				return true
			}
			if !seen[name] {
				seen[name] = true
				calls = append(calls, name)
			}
		case *ast.SelectorExpr:
			name := fun.Sel.Name
			if shouldSkipCall(name) {
				return true
			}
			if !seen[name] {
				seen[name] = true
				calls = append(calls, name)
			}
		}
		return true
	})
	sort.Strings(calls)
	return calls
}

func shouldSkipCall(name string) bool {
	switch name {
	case "", "Errorf", "Printf", "Sprintf", "Println", "Fatalf", "Panic", "NewReader", "NewWriter":
		return true
	default:
		return false
	}
}

func loadSQLTableNames(workspaceRoot string) []string {
	graph, err := LoadSQLGraph(workspaceStatePath(workspaceRoot, "sql_graph.json"))
	if err == nil && graph != nil {
		names := make([]string, 0, len(graph.Tables))
		for name := range graph.Tables {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}

	var names []string
	_ = filepath.WalkDir(workspaceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".sql") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		re := regexp.MustCompile(`(?i)create\s+table\s+(?:if\s+not\s+exists\s+)?([^\s(]+)`)
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if len(m) > 1 {
				names = append(names, strings.Trim(m[1], "`\""))
			}
		}
		return nil
	})
	sort.Strings(names)
	return uniqueStrings(names)
}

func collectTablesFromBody(body *ast.BlockStmt, tableNames []string) []string {
	if body == nil || len(tableNames) == 0 {
		return nil
	}
	tableSet := make(map[string]struct{}, len(tableNames))
	for _, t := range tableNames {
		tableSet[strings.ToLower(t)] = struct{}{}
	}
	found := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		text, _ := strings.CutPrefix(lit.Value, "`")
		text = strings.Trim(text, "\"'")
		lower := strings.ToLower(text)
		for t := range tableSet {
			if strings.Contains(lower, strings.ToLower(t)) {
				found[t] = true
			}
		}
		return true
	})
	out := make([]string, 0, len(found))
	for t := range found {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func routeFrameworkForFile(file string) string {
	return "generic"
}
