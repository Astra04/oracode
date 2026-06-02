package oracode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type ProtoMessageField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Number   int    `json:"number"`
	Repeated bool   `json:"repeated"`
}

type ProtoMessage struct {
	Name   string               `json:"name"`
	Fields []ProtoMessageField  `json:"fields"`
}

type ProtoServiceMethod struct {
	Name         string `json:"name"`
	RequestType  string `json:"requestType"`
	ResponseType string `json:"responseType"`
	ClientStream bool   `json:"clientStreaming"`
	ServerStream bool   `json:"serverStreaming"`
}

type ProtoService struct {
	Name    string               `json:"name"`
	File    string               `json:"file,omitempty"`
	Methods []ProtoServiceMethod `json:"methods"`
}

type ProtoGraph struct {
	Version   int                     `json:"version"`
	Generated string                  `json:"generated"`
	Services  map[string]ProtoService `json:"services"` // key = service name
	Messages  map[string]ProtoMessage `json:"messages"` // key = message name
}

func BuildProtoGraph(workspaceRoot string) (*ProtoGraph, error) {
	graph := &ProtoGraph{
		Version:   1,
		Generated: time.Now().Format(time.RFC3339),
		Services:  make(map[string]ProtoService),
		Messages:  make(map[string]ProtoMessage),
	}

	var protoFiles []string
	err := filepath.WalkDir(workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(path, ".proto") {
			protoFiles = append(protoFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, path := range protoFiles {
		if err := parseProtoFile(path, graph); err != nil {
			// Log error but continue with other files
			fmt.Fprintf(os.Stderr, "[oracode] proto parse error %s: %v\n", path, err)
		}
	}
	return graph, nil
}

func parseProtoFile(path string, graph *ProtoGraph) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var (
		inMessage      bool
		inService      bool
		currentMessage *ProtoMessage
		currentService *ProtoService
		lineNum        int
	)
	// Normalise path so callers can do consistent prefix checks
	normPath := filepath.ToSlash(path)
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		// Message start
		if strings.HasPrefix(line, "message ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				msgName := parts[1]
				msgName = strings.TrimSuffix(msgName, "{")
				currentMessage = &ProtoMessage{Name: msgName, Fields: []ProtoMessageField{}}
				inMessage = true
				inService = false
			}
			continue
		}

		// Service start
		if strings.HasPrefix(line, "service ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				svcName := parts[1]
				svcName = strings.TrimSuffix(svcName, "{")
				currentService = &ProtoService{Name: svcName, File: normPath, Methods: []ProtoServiceMethod{}}
				inService = true
				inMessage = false
			}
			continue
		}

		// Inside message: parse field
		if inMessage && currentMessage != nil {
			if line == "}" {
				graph.Messages[currentMessage.Name] = *currentMessage
				inMessage = false
				currentMessage = nil
				continue
			}
			// Parse field: [repeated] type name = number;
			fieldRe := regexp.MustCompile(`^(repeated\s+)?(\S+)\s+(\S+)\s*=\s*(\d+)\s*;`)
			if matches := fieldRe.FindStringSubmatch(line); matches != nil {
				repeated := matches[1] != ""
				fieldType := matches[2]
				fieldName := matches[3]
				fieldNumber := 0
				fmt.Sscanf(matches[4], "%d", &fieldNumber)
				currentMessage.Fields = append(currentMessage.Fields, ProtoMessageField{
					Name:     fieldName,
					Type:     fieldType,
					Number:   fieldNumber,
					Repeated: repeated,
				})
			}
			continue
		}

		// Inside service: parse rpc method
		if inService && currentService != nil {
			if line == "}" {
				graph.Services[currentService.Name] = *currentService
				inService = false
				currentService = nil
				continue
			}
			// rpc MethodName (RequestType) returns (ResponseType) [options] {}
			rpcRe := regexp.MustCompile(`rpc\s+(\w+)\s*\(\s*(\w+)\s*\)\s*returns\s*\(\s*(\w+)\s*\)`)
			if matches := rpcRe.FindStringSubmatch(line); matches != nil {
				method := ProtoServiceMethod{
					Name:         matches[1],
					RequestType:  matches[2],
					ResponseType: matches[3],
					ClientStream: false,
					ServerStream: false,
				}
				// Check for streaming
				if strings.Contains(line, "stream ") {
					if strings.Contains(line, "stream ") && strings.Index(line, "stream") < strings.Index(line, "returns") {
						method.ClientStream = true
					} else {
						method.ServerStream = true
					}
				}
				currentService.Methods = append(currentService.Methods, method)
			}
			continue
		}
	}
	return scanner.Err()
}

func (g *ProtoGraph) Save(path string) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func LoadProtoGraph(path string) (*ProtoGraph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var g ProtoGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
}
