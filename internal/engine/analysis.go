package engine

import (
	"fmt"
	"strings"

	"github.com/blevesearch/bleve/v2/analysis"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/simple"
	_ "github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
	_ "github.com/blevesearch/bleve/v2/analysis/char/html"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/de"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/en"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/es"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/fr"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/it"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/pt"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ru"
	_ "github.com/blevesearch/bleve/v2/analysis/token/edgengram"
	_ "github.com/blevesearch/bleve/v2/analysis/token/length"
	_ "github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	_ "github.com/blevesearch/bleve/v2/analysis/token/ngram"
	_ "github.com/blevesearch/bleve/v2/analysis/token/porter"
	_ "github.com/blevesearch/bleve/v2/analysis/token/reverse"
	_ "github.com/blevesearch/bleve/v2/analysis/token/shingle"
	_ "github.com/blevesearch/bleve/v2/analysis/token/snowball"
	_ "github.com/blevesearch/bleve/v2/analysis/token/stop"
	_ "github.com/blevesearch/bleve/v2/analysis/token/truncate"
	_ "github.com/blevesearch/bleve/v2/analysis/token/unicodenorm"
	_ "github.com/blevesearch/bleve/v2/analysis/token/unique"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/letter"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/regexp"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/single"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/web"
	_ "github.com/blevesearch/bleve/v2/analysis/tokenizer/whitespace"
	"github.com/blevesearch/bleve/v2/mapping"
)

// builtinAnalyzers maps OpenSearch built-in analyzer names to bleve names
// (registered globally or built lazily).
var builtinAnalyzers = map[string]string{
	"simple":     simple.Name,
	"keyword":    keyword.Name,
	"english":    "en",
	"german":     "de",
	"spanish":    "es",
	"french":     "fr",
	"italian":    "it",
	"portuguese": "pt",
	"russian":    "ru",
	"cjk":        "cjk",
	"kuromoji":   "cjk",
	"nori":       "cjk",
	"smartcn":    "cjk",
	"japanese":   "cjk",
	"korean":     "cjk",
	"chinese":    "cjk",
	"thai":       "cjk",
}

// japaneseAnalyzerType is the bleve analyzer type registered by the
// osmem/ja package (kagome). Empty when the package is not imported.
var japaneseAnalyzerType string

// RegisterJapaneseAnalyzer makes a bleve analyzer type the implementation of
// OpenSearch's kuromoji analyzer, tokenizer and token filters. The type's
// constructor receives the configuration described in the osmem/ja package.
func RegisterJapaneseAnalyzer(bleveType string) {
	japaneseAnalyzerType = bleveType
}

// kuromojiFilters are the token filters handled inside the Japanese analyzer.
var kuromojiFilters = map[string]bool{
	"kuromoji_baseform": true, "kuromoji_part_of_speech": true, "cjk_width": true, "ja_stop": true,
	"kuromoji_stemmer": true, "kuromoji_readingform": true, "kuromoji_number": true, "lowercase": true,
}

// analysisSet holds the analyzers available to an index.
type analysisSet struct {
	bmap     *mapping.IndexMappingImpl
	names    map[string]string // OpenSearch name -> bleve analyzer name
	warnings []string
}

// buildAnalysis creates the bleve index mapping for an index from its
// settings ("index.analysis.*").
func buildAnalysis(settings M, warn func(string)) (*analysisSet, error) {
	bm := mapping.NewIndexMapping()
	bm.DefaultField = "_all"
	bm.ScoringModel = "bm25"
	bm.IndexDynamic = false
	bm.StoreDynamic = false
	bm.DocValuesDynamic = false
	as := &analysisSet{bmap: bm, names: map[string]string{}}
	for k, v := range builtinAnalyzers {
		as.names[k] = v
	}
	// OpenSearch's standard analyzer has no stop words (bleve's does)
	if err := bm.AddCustomAnalyzer("os_standard", M{"type": "custom", "tokenizer": "unicode", "token_filters": []any{"to_lower"}}); err != nil {
		return nil, err
	}
	as.names["standard"] = "os_standard"
	bm.DefaultAnalyzer = "os_standard"
	// whitespace / stop are not registered globally in bleve
	if err := bm.AddCustomAnalyzer("os_whitespace", M{"type": "custom", "tokenizer": "whitespace"}); err != nil {
		return nil, err
	}
	as.names["whitespace"] = "os_whitespace"
	if err := bm.AddCustomAnalyzer("os_stop", M{"type": "custom", "tokenizer": "unicode", "token_filters": []any{"to_lower", "stop_en"}}); err != nil {
		return nil, err
	}
	as.names["stop"] = "os_stop"
	if err := bm.AddCustomAnalyzer("os_lowercase_keyword", M{"type": "custom", "tokenizer": "single", "token_filters": []any{"to_lower"}}); err != nil {
		return nil, err
	}
	as.names["lowercase"] = "os_lowercase_keyword"

	if japaneseAnalyzerType != "" {
		cfg := M{"type": japaneseAnalyzerType, "filters": []any{
			M{"type": "kuromoji_baseform"}, M{"type": "kuromoji_part_of_speech"}, M{"type": "cjk_width"},
			M{"type": "ja_stop"}, M{"type": "kuromoji_stemmer"}, M{"type": "lowercase"},
		}}
		if err := bm.AddCustomAnalyzer("os_analyzer_kuromoji", cfg); err != nil {
			return nil, err
		}
		for _, n := range []string{"kuromoji", "japanese"} {
			as.names[n] = "os_analyzer_kuromoji"
		}
	}
	an := analysisSettings(settings)
	if an == nil {
		return as, nil
	}
	// char filters
	for name, raw := range getMap(an, "char_filter") {
		spec, _ := raw.(M)
		cfg, ok := translateCharFilter(spec)
		if !ok {
			as.warn(warn, "char_filter [%s] of type [%s] is not supported; ignored", name, getString(spec, "type"))
			continue
		}
		if err := bm.AddCustomCharFilter(name, cfg); err != nil {
			return nil, errIllegalArgument("char_filter [%s]: %v", name, err)
		}
	}
	// tokenizers
	tokenizerFilters := map[string][]string{} // extra token filters implied by tokenizer emulation
	for name, raw := range getMap(an, "tokenizer") {
		spec, _ := raw.(M)
		if getString(spec, "type") == "kuromoji_tokenizer" && japaneseAnalyzerType != "" {
			continue // handled when an analyzer references it
		}
		cfg, filters, ok := translateTokenizer(spec)
		if !ok {
			as.warn(warn, "tokenizer [%s] of type [%s] is not supported; using standard", name, getString(spec, "type"))
			cfg = M{"type": "unicode"}
		}
		if err := bm.AddCustomTokenizer(name, cfg); err != nil {
			return nil, errIllegalArgument("tokenizer [%s]: %v", name, err)
		}
		if len(filters) > 0 {
			for i, f := range filters {
				fname := fmt.Sprintf("%s__tok%d", name, i)
				if err := bm.AddCustomTokenFilter(fname, f); err != nil {
					return nil, errIllegalArgument("tokenizer [%s]: %v", name, err)
				}
				tokenizerFilters[name] = append(tokenizerFilters[name], fname)
			}
		}
	}
	// token filters
	for name, raw := range getMap(an, "filter") {
		spec, _ := raw.(M)
		if kuromojiFilters[getString(spec, "type")] && japaneseAnalyzerType != "" && getString(spec, "type") != "lowercase" {
			continue // handled inside the Japanese analyzer
		}
		cfg, ok := translateTokenFilter(spec)
		if !ok {
			as.warn(warn, "token filter [%s] of type [%s] is not supported; ignored", name, getString(spec, "type"))
			continue
		}
		if cfg == nil {
			continue // maps to nothing (e.g. trim)
		}
		if err := bm.AddCustomTokenFilter(name, cfg); err != nil {
			return nil, errIllegalArgument("filter [%s]: %v", name, err)
		}
	}
	// normalizers become keyword analyzers with filters
	for name, raw := range getMap(an, "normalizer") {
		spec, _ := raw.(M)
		cfg := M{"type": "custom", "tokenizer": "single"}
		cfs, tfs := as.resolveFilters(bm, spec, warn)
		if len(cfs) > 0 {
			cfg["char_filters"] = cfs
		}
		if len(tfs) > 0 {
			cfg["token_filters"] = tfs
		}
		bname := "os_normalizer_" + name
		if err := bm.AddCustomAnalyzer(bname, cfg); err != nil {
			return nil, errIllegalArgument("normalizer [%s]: %v", name, err)
		}
		as.names["normalizer:"+name] = bname
	}
	// analyzers
	for name, raw := range getMap(an, "analyzer") {
		spec, _ := raw.(M)
		typ := getString(spec, "type")
		if typ == "" {
			typ = "custom"
		}
		var cfg M
		switch typ {
		case "custom":
			tok := getString(spec, "tokenizer")
			if tok == "" {
				tok = "standard"
			}
			if jcfg, ok := as.japaneseConfig(an, tok, spec, warn); ok {
				bname := "os_analyzer_" + name
				if err := bm.AddCustomAnalyzer(bname, jcfg); err != nil {
					return nil, errIllegalArgument("analyzer [%s]: %v", name, err)
				}
				as.names[name] = bname
				continue
			}
			btok := tok
			if _, custom := getMap(an, "tokenizer")[tok]; !custom {
				var ok bool
				btok, ok = builtinTokenizers[tok]
				if !ok {
					as.warn(warn, "analyzer [%s]: tokenizer [%s] is not supported; using standard", name, tok)
					btok = "unicode"
				}
			}
			cfg = M{"type": "custom", "tokenizer": btok}
			cfs, tfs := as.resolveFilters(bm, spec, warn)
			if extra := tokenizerFilters[tok]; len(extra) > 0 {
				tfs = append(toAnyList(extra), tfs...)
			}
			if btok == "ngram_tok" || btok == "edge_ngram_tok" {
				// emulated ngram tokenizers: single token + ngram filter
			}
			if len(cfs) > 0 {
				cfg["char_filters"] = cfs
			}
			if len(tfs) > 0 {
				cfg["token_filters"] = tfs
			}
		case "standard":
			cfg = M{"type": "custom", "tokenizer": "unicode", "token_filters": []any{"to_lower"}}
			if sw, ok := spec["stopwords"]; ok {
				fname := "os_stop_" + name
				if err := bm.AddCustomTokenFilter(fname, stopFilterConfig(sw)); err == nil {
					cfg["token_filters"] = []any{"to_lower", fname}
				}
			}
		case "pattern":
			pat := getString(spec, "pattern")
			if pat == "" {
				pat = `\W+`
			}
			tname := "os_pattern_tok_" + name
			if err := bm.AddCustomTokenizer(tname, M{"type": "regexp", "regexp": pat}); err != nil {
				return nil, errIllegalArgument("analyzer [%s]: %v", name, err)
			}
			cfg = M{"type": "custom", "tokenizer": tname}
			if getBool(spec, "lowercase", true) {
				cfg["token_filters"] = []any{"to_lower"}
			}
		case "stop":
			fname := "os_stop_" + name
			sw, ok := spec["stopwords"]
			if !ok {
				sw = "_english_"
			}
			if err := bm.AddCustomTokenFilter(fname, stopFilterConfig(sw)); err != nil {
				return nil, errIllegalArgument("analyzer [%s]: %v", name, err)
			}
			cfg = M{"type": "custom", "tokenizer": "letter", "token_filters": []any{"to_lower", fname}}
		default:
			if bname, ok := builtinAnalyzers[typ]; ok {
				as.names[name] = bname
				continue
			}
			as.warn(warn, "analyzer [%s] of type [%s] is not supported; using standard", name, typ)
			as.names[name] = "os_standard"
			continue
		}
		bname := "os_analyzer_" + name
		if err := bm.AddCustomAnalyzer(bname, cfg); err != nil {
			return nil, errIllegalArgument("analyzer [%s]: %v", name, err)
		}
		as.names[name] = bname
	}
	return as, nil
}

// japaneseConfig builds the configuration of the Japanese analyzer for a
// custom analyzer whose tokenizer is kuromoji_tokenizer (built-in or
// defined in settings). ok is false when the analyzer is not Japanese or
// the osmem/ja package is not loaded.
func (as *analysisSet) japaneseConfig(an M, tok string, spec M, warn func(string)) (M, bool) {
	if japaneseAnalyzerType == "" {
		return nil, false
	}
	tokSpec, custom := getMap(an, "tokenizer")[tok].(M)
	if custom {
		if getString(tokSpec, "type") != "kuromoji_tokenizer" {
			return nil, false
		}
	} else if tok != "kuromoji_tokenizer" {
		return nil, false
	}
	cfg := M{"type": japaneseAnalyzerType}
	for _, k := range []string{"mode", "discard_punctuation", "user_dictionary", "user_dictionary_rules", "discard_compound_token"} {
		if v, ok := tokSpec[k]; ok {
			cfg[k] = v
		}
	}
	var filters []any
	var extra []any
	customFilters := getMap(an, "filter")
	for _, name := range getStrings(spec, "filter") {
		fspec, isCustom := customFilters[name].(M)
		typ := name
		if isCustom {
			typ = getString(fspec, "type")
		}
		if kuromojiFilters[typ] {
			f := M{"type": typ}
			for k, v := range fspec {
				if k != "type" {
					f[k] = v
				}
			}
			filters = append(filters, f)
			continue
		}
		if len(filters) == 0 && len(extra) == 0 {
			// non-kuromoji filters before kuromoji ones: keep order by
			// treating them as trailing filters (approximation)
			as.warn(warn, "filter [%s] is applied after the kuromoji filters", name)
		}
		_, tfs := as.resolveFilters(as.bmap, M{"filter": []any{name}}, warn)
		extra = append(extra, tfs...)
	}
	cfs, _ := as.resolveFilters(as.bmap, M{"char_filter": spec["char_filter"]}, warn)
	cfg["filters"] = filters
	if len(extra) > 0 {
		cfg["token_filters"] = extra
	}
	if len(cfs) > 0 {
		cfg["char_filters"] = cfs
	}
	return cfg, true
}

func (as *analysisSet) warn(warn func(string), format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	as.warnings = append(as.warnings, msg)
	if warn != nil {
		warn(msg)
	}
}

func toAnyList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// analysisSettings finds the analysis block in settings (flat or nested).
func analysisSettings(settings M) M {
	if settings == nil {
		return nil
	}
	if idx := getMap(settings, "index"); idx != nil {
		if an := getMap(idx, "analysis"); an != nil {
			return an
		}
	}
	if an := getMap(settings, "analysis"); an != nil {
		return an
	}
	if an := getMap(settings, "index.analysis"); an != nil {
		return an
	}
	return nil
}

var builtinTokenizers = map[string]string{
	"standard":           "unicode",
	"classic":            "unicode",
	"whitespace":         "whitespace",
	"keyword":            "single",
	"letter":             "letter",
	"lowercase":          "letter",
	"uax_url_email":      "web",
	"icu_tokenizer":      "unicode",
	"kuromoji_tokenizer": "unicode",
}

var builtinTokenFilters = map[string]string{
	"lowercase":          "to_lower",
	"stop":               "stop_en",
	"porter_stem":        "stemmer_porter",
	"kstem":              "stemmer_porter",
	"unique":             "unique",
	"reverse":            "reverse",
	"cjk_bigram":         "cjk_bigram",
	"cjk_width":          "cjk_width",
	"apostrophe":         "apostrophe",
	"english_possessive": "possessive_en",
	"asciifolding":       "os_asciifolding_token",
}

// translateCharFilter converts an OpenSearch char filter definition.
func translateCharFilter(spec M) (M, bool) {
	switch getString(spec, "type") {
	case "html_strip":
		return M{"type": "html"}, true
	case "pattern_replace":
		return M{"type": "regexp", "regexp": getString(spec, "pattern"), "replace": getString(spec, "replacement")}, true
	case "mapping":
		// emulate a small mapping with regexp replacements is not possible; ignore
		return nil, false
	}
	return nil, false
}

// translateTokenizer converts a tokenizer definition. Some OpenSearch
// tokenizers (ngram) are emulated with a tokenizer plus token filters.
func translateTokenizer(spec M) (M, []M, bool) {
	typ := getString(spec, "type")
	switch typ {
	case "standard", "classic", "icu_tokenizer", "kuromoji_tokenizer":
		return M{"type": "unicode"}, nil, true
	case "whitespace":
		return M{"type": "whitespace"}, nil, true
	case "keyword":
		return M{"type": "single"}, nil, true
	case "letter", "lowercase":
		return M{"type": "letter"}, nil, true
	case "uax_url_email":
		return M{"type": "web"}, nil, true
	case "pattern":
		pat := getString(spec, "pattern")
		if pat == "" {
			pat = `\W+`
		}
		return M{"type": "regexp", "regexp": pat}, nil, true
	case "ngram", "edge_ngram":
		min := getInt(spec, "min_gram", 1)
		max := getInt(spec, "max_gram", 2)
		if typ == "edge_ngram" && !hasKey(spec, "max_gram") {
			max = 2
		}
		var tf M
		if typ == "ngram" {
			tf = M{"type": "ngram", "min": float64(min), "max": float64(max)}
		} else {
			tf = M{"type": "edge_ngram", "min": float64(min), "max": float64(max), "back": false}
		}
		tokenChars := getStrings(spec, "token_chars")
		if len(tokenChars) == 0 {
			return M{"type": "single"}, []M{tf}, true
		}
		// split on characters not in token_chars, then n-gram each token
		return M{"type": "unicode"}, []M{tf}, true
	case "char_group":
		chars := getStrings(spec, "tokenize_on_chars")
		var sb strings.Builder
		sb.WriteString("[")
		for _, c := range chars {
			switch c {
			case "whitespace":
				sb.WriteString(`\s`)
			case "letter":
				sb.WriteString(`\p{L}`)
			case "digit":
				sb.WriteString(`\d`)
			case "punctuation":
				sb.WriteString(`\p{P}`)
			case "symbol":
				sb.WriteString(`\p{S}`)
			default:
				sb.WriteString(regexpEscape(c))
			}
		}
		sb.WriteString("]+")
		return M{"type": "regexp", "regexp": sb.String()}, nil, true
	}
	return nil, nil, false
}

func regexpEscape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\^$.|?*+()[]{}-`, r) {
			sb.WriteRune('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func hasKey(m M, key string) bool {
	_, ok := m[key]
	return ok
}

// translateTokenFilter converts a token filter definition. A nil config with
// ok=true means the filter has no effect in bleve and can be dropped.
func translateTokenFilter(spec M) (M, bool) {
	typ := getString(spec, "type")
	switch typ {
	case "lowercase":
		return M{"type": "to_lower"}, true
	case "asciifolding":
		return M{"type": "unicodenorm", "form": "nfkd"}, true
	case "stop":
		sw, ok := spec["stopwords"]
		if !ok {
			sw = "_english_"
		}
		return stopFilterConfig(sw), true
	case "ngram":
		return M{"type": "ngram", "min": float64(getInt(spec, "min_gram", 1)), "max": float64(getInt(spec, "max_gram", 2))}, true
	case "edge_ngram":
		back := getString(spec, "side") == "back"
		return M{"type": "edge_ngram", "min": float64(getInt(spec, "min_gram", 1)), "max": float64(getInt(spec, "max_gram", 2)), "back": back}, true
	case "shingle":
		min := getInt(spec, "min_shingle_size", 2)
		max := getInt(spec, "max_shingle_size", 2)
		cfg := M{"type": "shingle", "min": float64(min), "max": float64(max), "output_original": getBool(spec, "output_unigrams", true)}
		if sep := getString(spec, "token_separator"); sep != "" {
			cfg["separator"] = sep
		}
		return cfg, true
	case "stemmer", "snowball":
		lang := getString(spec, "language")
		if lang == "" {
			lang = getString(spec, "name")
		}
		if lang == "" {
			lang = "english"
		}
		lang = strings.ToLower(lang)
		switch lang {
		case "porter", "porter2", "english", "light_english", "minimal_english":
			return M{"type": "stemmer_porter"}, true
		}
		return M{"type": "stemmer_snowball", "language": lang}, true
	case "porter_stem", "kstem":
		return M{"type": "stemmer_porter"}, true
	case "truncate":
		return M{"type": "truncate_token", "length": float64(getInt(spec, "length", 10))}, true
	case "length":
		cfg := M{"type": "length"}
		if hasKey(spec, "min") {
			cfg["min"] = float64(getInt(spec, "min", 0))
		}
		if hasKey(spec, "max") {
			cfg["max"] = float64(getInt(spec, "max", 0))
		}
		return cfg, true
	case "unique":
		return M{"type": "unique"}, true
	case "reverse":
		return M{"type": "reverse"}, true
	case "trim", "decimal_digit", "keyword_repeat", "remove_duplicates", "flatten_graph":
		return nil, true
	case "cjk_bigram":
		return M{"type": "cjk_bigram"}, true
	case "cjk_width":
		return M{"type": "cjk_width"}, true
	case "apostrophe":
		return M{"type": "apostrophe"}, true
	}
	return nil, false
}

func stopFilterConfig(sw any) M {
	words := getList(sw)
	tokens := M{}
	for _, w := range words {
		s, _ := w.(string)
		if s == "_english_" || s == "_none_" {
			continue
		}
		if s != "" {
			tokens[s] = true
		}
	}
	if s, ok := sw.(string); ok && s == "_english_" {
		return M{"type": "stop_en"}
	}
	if len(tokens) == 0 {
		return M{"type": "stop_tokens", "stop_token_map": "os_empty_stop"}
	}
	return M{"type": "os_stop_list", "tokens": tokens}
}

// resolveFilters resolves char_filter and filter lists of an analyzer or
// normalizer spec to bleve names, registering builtin emulations as needed.
func (as *analysisSet) resolveFilters(bm *mapping.IndexMappingImpl, spec M, warn func(string)) (charFilters []any, tokenFilters []any) {
	for _, cf := range getStrings(spec, "char_filter") {
		if _, ok := bm.CustomAnalysis.CharFilters[cf]; ok {
			charFilters = append(charFilters, cf)
			continue
		}
		switch cf {
		case "html_strip":
			charFilters = append(charFilters, "html")
		default:
			as.warn(warn, "char_filter [%s] is not supported; ignored", cf)
		}
	}
	for _, tf := range getStrings(spec, "filter") {
		if _, ok := bm.CustomAnalysis.TokenFilters[tf]; ok {
			tokenFilters = append(tokenFilters, tf)
			continue
		}
		if b, ok := builtinTokenFilters[tf]; ok {
			if b == "os_asciifolding_token" {
				if _, ok := bm.CustomAnalysis.TokenFilters[b]; !ok {
					_ = bm.AddCustomTokenFilter(b, M{"type": "unicodenorm", "form": "nfkd"})
				}
			}
			tokenFilters = append(tokenFilters, b)
			continue
		}
		if cfg, ok := translateTokenFilter(M{"type": tf}); ok {
			if cfg == nil {
				continue
			}
			name := "os_builtin_" + tf
			if _, ok := bm.CustomAnalysis.TokenFilters[name]; !ok {
				if err := bm.AddCustomTokenFilter(name, cfg); err != nil {
					as.warn(warn, "token filter [%s]: %v", tf, err)
					continue
				}
			}
			tokenFilters = append(tokenFilters, name)
			continue
		}
		as.warn(warn, "token filter [%s] is not supported; ignored", tf)
	}
	return
}

// analyzerNamed returns the bleve analyzer for an OpenSearch analyzer name.
func (as *analysisSet) analyzerNamed(name string) (analysis.Analyzer, error) {
	if name == "" {
		name = "standard"
	}
	bname, ok := as.names[name]
	if !ok {
		bname = name
	}
	a := as.bmap.AnalyzerNamed(bname)
	if a == nil {
		return nil, errIllegalArgument("analyzer [%s] has not been configured in mappings", name)
	}
	return a, nil
}

// bleveAnalyzerName returns the bleve-side name for an analyzer.
func (as *analysisSet) bleveAnalyzerName(name string) string {
	if name == "" {
		name = "standard"
	}
	if b, ok := as.names[name]; ok {
		return b
	}
	return name
}

func (as *analysisSet) normalizerNamed(name string) (analysis.Analyzer, error) {
	bname := keyword.Name
	switch {
	case name == "":
	case name == "lowercase":
		bname = "os_lowercase_keyword"
	default:
		b, ok := as.names["normalizer:"+name]
		if !ok {
			return nil, errIllegalArgument("normalizer [%s] has not been configured in mappings", name)
		}
		bname = b
	}
	a := as.bmap.AnalyzerNamed(bname)
	if a == nil {
		return nil, errIllegalArgument("normalizer [%s] has not been configured in mappings", name)
	}
	return a, nil
}
