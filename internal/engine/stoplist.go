package engine

import (
	"fmt"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/token/stop"
	"github.com/blevesearch/bleve/v2/registry"
)

// os_stop_list is a stop token filter with an inline word list.
func init() {
	registry.RegisterTokenFilter("os_stop_list", func(config map[string]interface{}, cache *registry.Cache) (analysis.TokenFilter, error) {
		tokens, ok := config["tokens"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("os_stop_list: tokens must be an object")
		}
		tm := analysis.TokenMap{}
		for k := range tokens {
			tm.AddToken(k)
		}
		return stop.NewStopTokensFilter(tm), nil
	})
	registry.RegisterTokenMap("os_empty_stop", func(config map[string]interface{}, cache *registry.Cache) (analysis.TokenMap, error) {
		return analysis.TokenMap{}, nil
	})
}
