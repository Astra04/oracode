package oracode

import (
	"encoding/json"
	"os"
	"time"
)

type EffectGraph struct {
	Version   int                       `json:"version"`
	Generated time.Time                 `json:"generated"`
	Symbols   map[string]*EffectModality `json:"symbols"`
}

func (g *EffectGraph) Save(path string) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func LoadEffectGraph(path string) (*EffectGraph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var g EffectGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
}
