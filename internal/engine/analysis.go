package engine

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/blevesearch/bleve/v2/analysis"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/simple"
	_ "github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
	bleveasciifolding "github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
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
	"github.com/blevesearch/bleve/v2/registry"

	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"regexp/syntax"
	"sort"
	"strconv"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	blevear "github.com/blevesearch/bleve/v2/analysis/lang/ar"
	blevebg "github.com/blevesearch/bleve/v2/analysis/lang/bg"
	bleveca "github.com/blevesearch/bleve/v2/analysis/lang/ca"
	blevecjk "github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	bleveckb "github.com/blevesearch/bleve/v2/analysis/lang/ckb"
	blevecs "github.com/blevesearch/bleve/v2/analysis/lang/cs"
	bleveda "github.com/blevesearch/bleve/v2/analysis/lang/da"
	blevede "github.com/blevesearch/bleve/v2/analysis/lang/de"
	bleveel "github.com/blevesearch/bleve/v2/analysis/lang/el"
	blevees "github.com/blevesearch/bleve/v2/analysis/lang/es"
	bleveeu "github.com/blevesearch/bleve/v2/analysis/lang/eu"
	blevefa "github.com/blevesearch/bleve/v2/analysis/lang/fa"
	blevefi "github.com/blevesearch/bleve/v2/analysis/lang/fi"
	blevefr "github.com/blevesearch/bleve/v2/analysis/lang/fr"
	blevega "github.com/blevesearch/bleve/v2/analysis/lang/ga"
	blevegl "github.com/blevesearch/bleve/v2/analysis/lang/gl"
	blevehi "github.com/blevesearch/bleve/v2/analysis/lang/hi"
	blevehu "github.com/blevesearch/bleve/v2/analysis/lang/hu"
	blevehy "github.com/blevesearch/bleve/v2/analysis/lang/hy"
	bleveid "github.com/blevesearch/bleve/v2/analysis/lang/id"
	blevein "github.com/blevesearch/bleve/v2/analysis/lang/in"
	bleveit "github.com/blevesearch/bleve/v2/analysis/lang/it"
	blevenl "github.com/blevesearch/bleve/v2/analysis/lang/nl"
	bleveno "github.com/blevesearch/bleve/v2/analysis/lang/no"
	blevept "github.com/blevesearch/bleve/v2/analysis/lang/pt"
	blevero "github.com/blevesearch/bleve/v2/analysis/lang/ro"
	bleveru "github.com/blevesearch/bleve/v2/analysis/lang/ru"
	blevesv "github.com/blevesearch/bleve/v2/analysis/lang/sv"
	blevetr "github.com/blevesearch/bleve/v2/analysis/lang/tr"
	porterstemmer "github.com/blevesearch/go-porterstemmer"
	"github.com/blevesearch/snowballstem"
	"github.com/blevesearch/snowballstem/danish"
	"github.com/blevesearch/snowballstem/dutch"
	"github.com/blevesearch/snowballstem/english"
	"github.com/blevesearch/snowballstem/finnish"
	"github.com/blevesearch/snowballstem/french"
	"github.com/blevesearch/snowballstem/german"
	"github.com/blevesearch/snowballstem/hungarian"
	"github.com/blevesearch/snowballstem/irish"
	"github.com/blevesearch/snowballstem/italian"
	"github.com/blevesearch/snowballstem/norwegian"
	"github.com/blevesearch/snowballstem/porter"
	"github.com/blevesearch/snowballstem/portuguese"
	"github.com/blevesearch/snowballstem/romanian"
	"github.com/blevesearch/snowballstem/russian"
	"github.com/blevesearch/snowballstem/spanish"
	"github.com/blevesearch/snowballstem/swedish"
	"github.com/blevesearch/snowballstem/turkish"
)

type asciiFoldingTokenFilter struct {
	folding          *bleveasciifolding.AsciiFoldingFilter
	preserveOriginal bool
}

func (f *asciiFoldingTokenFilter) Filter(input analysis.TokenStream) analysis.TokenStream {
	output := make(analysis.TokenStream, 0, len(input))
	for _, token := range input {
		original := token.Term
		folded := f.folding.Filter(original)
		token.Term = folded
		output = append(output, token)
		if f.preserveOriginal && !bytes.Equal(original, folded) {
			originalToken := *token
			originalToken.Term = original
			output = append(output, &originalToken)
		}
	}
	return output
}

func asciiFoldingTokenFilterConstructor(config map[string]interface{}, _ *registry.Cache) (analysis.TokenFilter, error) {
	return &asciiFoldingTokenFilter{folding: bleveasciifolding.New(), preserveOriginal: getBool(M(config), "preserve_original", false)}, nil
}

func init() {
	if err := registry.RegisterTokenFilter("asciifolding", asciiFoldingTokenFilterConstructor); err != nil {
		panic(err)
	}
}

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
	if err := validateAnalysis(settings, nil); err != nil {
		return nil, err
	}
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
		// Keep runs composed of token_chars, then n-gram each run. Bleve
		// has no token_chars option on its ngram tokenizer, so use its regexp
		// tokenizer for the same boundary behavior.
		return M{"type": "regexp", "regexp": ngramTokenBoundaryPattern(tokenChars, getStrings(spec, "custom_token_chars"))}, []M{tf}, true
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

func ngramTokenBoundaryPattern(tokenChars, customChars []string) string {
	var allowed strings.Builder
	for _, tokenChar := range tokenChars {
		switch tokenChar {
		case "letter":
			allowed.WriteString(`\p{L}`)
		case "digit":
			allowed.WriteString(`\p{N}`)
		case "whitespace":
			allowed.WriteString(`\p{Z}\t\n\v\f\r`)
		case "punctuation":
			allowed.WriteString(`\p{P}`)
		case "symbol":
			allowed.WriteString(`\p{S}`)
		}
	}
	for _, custom := range customChars {
		for _, r := range custom {
			if strings.ContainsRune(`\^-]`, r) {
				allowed.WriteRune('\\')
			}
			allowed.WriteRune(r)
		}
	}
	return `[` + allowed.String() + `]+`
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
		return M{"type": "asciifolding", "preserve_original": getBool(spec, "preserve_original", false)}, true
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
				// the built-in asciifolding filter is the registered
				// asciifolding token filter with default options
				if _, ok := bm.CustomAnalysis.TokenFilters[b]; !ok {
					if err := bm.AddCustomTokenFilter(b, M{"type": "asciifolding"}); err != nil {
						as.warn(warn, "token filter [%s]: %v", tf, err)
						continue
					}
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

// ---------------------------------------------------------------------------
// OpenSearch-compatible analysis chain
//
// The _analyze API and the analyzers used for indexing run the same chain of
// char filters, a tokenizer and token filters, modelled on Lucene's
// TokenStream contract: tokens carry position increments, position lengths,
// types and UTF-16 (Java char) offsets into the original text.
// ---------------------------------------------------------------------------

// anToken is one token of an analysis chain.
type anToken struct {
	term    string
	start   int // UTF-16 offsets into the original text
	end     int
	posInc  int
	posLen  int
	typ     string
	keyword bool
	payload []byte
}

// anStream is the output of a tokenizer or a token filter. finalOffset and
// finalPosInc are the offset and position increment reported by end().
type anStream struct {
	tokens      []anToken
	finalOffset int
	finalPosInc int
}

type anCharFilter interface {
	filter(text string) (string, *offsetMap)
}

type anTokenizer interface {
	tokenize(text string) anStream
}

type anTokenFilter interface {
	apply(in anStream) anStream
}

// token attributes that components add to Lucene's attribute source; they
// show up in explain output
const (
	attrKeyword = 1 << iota
	attrPayload
)

type namedCharFilter struct {
	name string
	cf   anCharFilter
}

type namedTokenizer struct {
	name string
	tok  anTokenizer
}

type namedTokenFilter struct {
	name  string
	tf    anTokenFilter
	attrs int
}

// anAnalyzer is an analyzer: char filters, a tokenizer and token filters.
type anAnalyzer struct {
	name        string
	custom      bool // a custom analyzer: explain lists the components
	charFilters []namedCharFilter
	tokenizer   namedTokenizer
	filters     []namedTokenFilter
	posGap      int
	offsetGap   int
}

// offsetMap is Lucene's BaseCharFilter offset correction map.
type offsetMap struct {
	offs  []int
	diffs []int
}

func (m *offsetMap) add(off, cumulativeDiff int) {
	if n := len(m.offs); n > 0 && m.offs[n-1] == off {
		m.diffs[n-1] = cumulativeDiff
		return
	}
	m.offs = append(m.offs, off)
	m.diffs = append(m.diffs, cumulativeDiff)
}

func (m *offsetMap) lastDiff() int {
	if len(m.diffs) == 0 {
		return 0
	}
	return m.diffs[len(m.diffs)-1]
}

func (m *offsetMap) correct(off int) int {
	if m == nil || len(m.offs) == 0 {
		return off
	}
	i := sort.SearchInts(m.offs, off+1) - 1
	if i < 0 {
		return off
	}
	return off + m.diffs[i]
}

func correctOffsets(maps []*offsetMap, off int) int {
	for i := len(maps) - 1; i >= 0; i-- {
		off = maps[i].correct(off)
	}
	return off
}

// run applies the char filters, the tokenizer and the first n token filters
// (all of them when n < 0). filtered receives the output of each char filter.
func (a *anAnalyzer) run(text string, n int, filtered []string) anStream {
	var maps []*offsetMap
	for i, cf := range a.charFilters {
		var m *offsetMap
		text, m = cf.cf.filter(text)
		if filtered != nil {
			filtered[i] = text
		}
		if m != nil && len(m.offs) > 0 {
			maps = append(maps, m)
		}
	}
	s := a.tokenizer.tok.tokenize(text)
	if len(maps) > 0 {
		for i := range s.tokens {
			s.tokens[i].start = correctOffsets(maps, s.tokens[i].start)
			s.tokens[i].end = correctOffsets(maps, s.tokens[i].end)
		}
		s.finalOffset = correctOffsets(maps, s.finalOffset)
	}
	if n < 0 || n > len(a.filters) {
		n = len(a.filters)
	}
	for i := 0; i < n; i++ {
		s = a.filters[i].tf.apply(s)
	}
	return s
}

func (a *anAnalyzer) analyze(text string) anStream { return a.run(text, -1, nil) }

// attrs returns the extra attributes present after the first n filters.
func (a *anAnalyzer) attrs(n int) int {
	out := 0
	for i := 0; i < n && i < len(a.filters); i++ {
		out |= a.filters[i].attrs
	}
	return out
}

// tokenList accumulates the tokens of several values the way
// TransportAnalyzeAction does (positions and offsets continue across values).
type tokenList struct {
	lastPosition int
	lastOffset   int
	tokens       []any
	include      map[string]bool // explain attributes filter (nil: plain output)
	explain      bool
	attrs        int
	count        *int
	max          int
}

func newTokenList(explain bool, include map[string]bool, attrs int, count *int, max int) *tokenList {
	return &tokenList{lastPosition: -1, explain: explain, include: include, attrs: attrs, tokens: []any{}, count: count, max: max}
}

func (l *tokenList) add(s anStream, posGap, offsetGap int) error {
	for _, t := range s.tokens {
		if t.posInc > 0 {
			l.lastPosition += t.posInc
		}
		*l.count++
		if *l.count > l.max {
			return &Error{Status: 500, Type: "illegal_state_exception", Reason: fmt.Sprintf("The number of tokens produced by calling _analyze has exceeded the allowed maximum of [%d]. This limit can be set by changing the [index.analyze.max_token_count] index level setting.", l.max)}
		}
		l.tokens = append(l.tokens, l.render(t))
	}
	l.lastOffset += s.finalOffset
	l.lastPosition += s.finalPosInc + posGap
	l.lastOffset += offsetGap
	return nil
}

func (l *tokenList) render(t anToken) M {
	posLen := t.posLen
	if posLen < 1 {
		posLen = 1
	}
	m := M{"token": t.term, "start_offset": l.lastOffset + t.start, "end_offset": l.lastOffset + t.end, "type": t.typ, "position": l.lastPosition}
	if !l.explain {
		if posLen > 1 {
			m["positionLength"] = posLen
		}
		return m
	}
	if posLen > 1 {
		m["positionLength"] = posLen
	}
	want := func(k string) bool { return len(l.include) == 0 || l.include[k] }
	if want("bytes") {
		m["bytes"] = bytesRefString([]byte(t.term))
	}
	if l.attrs&attrKeyword != 0 && want("keyword") {
		m["keyword"] = t.keyword
	}
	if l.attrs&attrPayload != 0 && want("payload") {
		if t.payload == nil {
			m["payload"] = nil
		} else {
			m["payload"] = bytesRefString(t.payload)
		}
	}
	if want("positionlength") {
		m["positionLength"] = posLen
	}
	if want("termfrequency") {
		m["termFrequency"] = 1
	}
	return m
}

// bytesRefString renders bytes like Lucene's BytesRef.toString.
func bytesRefString(b []byte) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strconv.FormatInt(int64(c), 16))
	}
	sb.WriteByte(']')
	return sb.String()
}

// utf16Len is the length of s in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// runeText is a text as code points with their UTF-16 offsets.
type runeText struct {
	runes []rune
	u16   []int // u16[i] is the UTF-16 offset of runes[i]; len(runes)+1 entries
}

func newRuneText(s string) runeText {
	rs := []rune(s)
	u := make([]int, len(rs)+1)
	off := 0
	for i, r := range rs {
		u[i] = off
		if r >= 0x10000 {
			off += 2
		} else {
			off++
		}
	}
	u[len(rs)] = off
	return runeText{runes: rs, u16: u}
}

func (t runeText) str(i, j int) string { return string(t.runes[i:j]) }

// byteOffsets maps UTF-16 offsets of s to byte offsets.
func byteOffsets(s string) func(int) int {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return func(off int) int {
			if off > len(s) {
				return len(s)
			}
			if off < 0 {
				return 0
			}
			return off
		}
	}
	table := make([]int, 0, len(s)+1)
	for i, r := range s {
		table = append(table, i)
		if r >= 0x10000 {
			table = append(table, i)
		}
	}
	table = append(table, len(s))
	return func(off int) int {
		if off < 0 {
			return 0
		}
		if off >= len(table) {
			return len(s)
		}
		return table[off]
	}
}

// ---------------------------------------------------------------------------
// bleve adapter: runs an analysis chain for indexing and search
// ---------------------------------------------------------------------------

const osmemChainAnalyzerType = "osmem_chain"

// chainRef carries a chain through bleve's (JSON-marshalled) analyzer config.
type chainRef struct{ a *anAnalyzer }

func (chainRef) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

type bleveChainAnalyzer struct{ a *anAnalyzer }

func (b *bleveChainAnalyzer) Analyze(input []byte) analysis.TokenStream {
	text := string(input)
	s := b.a.analyze(text)
	conv := byteOffsets(text)
	out := make(analysis.TokenStream, 0, len(s.tokens))
	pos := 0
	for _, t := range s.tokens {
		pos += t.posInc
		if pos < 1 {
			pos = 1
		}
		typ := analysis.AlphaNumeric
		switch t.typ {
		case "<NUM>":
			typ = analysis.Numeric
		case "<IDEOGRAPHIC>", "<HIRAGANA>", "<KATAKANA>", "<HANGUL>", "<CJ>":
			typ = analysis.Ideographic
		case "shingle":
			typ = analysis.Shingle
		case "<SINGLE>":
			typ = analysis.Single
		case "<DOUBLE>":
			typ = analysis.Double
		}
		out = append(out, &analysis.Token{Term: []byte(t.term), Start: conv(t.start), End: conv(t.end), Position: pos, Type: typ, KeyWord: t.keyword})
	}
	return out
}

func init() {
	if err := registry.RegisterAnalyzer(osmemChainAnalyzerType, func(config map[string]interface{}, _ *registry.Cache) (analysis.Analyzer, error) {
		ref, ok := config["chain"].(chainRef)
		if !ok || ref.a == nil {
			return nil, fmt.Errorf("osmem_chain: missing chain")
		}
		return &bleveChainAnalyzer{a: ref.a}, nil
	}); err != nil {
		panic(err)
	}
}

// ---------------------------------------------------------------------------
// tokenizers
// ---------------------------------------------------------------------------

// jflexDFA holds the scanner tables of a JFlex-generated Lucene tokenizer
// (StandardTokenizerImpl, ClassicTokenizerImpl, UAX29URLEmailTokenizerImpl).
type jflexDFA struct {
	top      []uint16
	blocks   []byte
	action   []byte
	rowmap   []uint32
	trans    []uint16 // next state + 1 (0: no transition)
	attr     []byte
	lexstate []int
}

func loadJFlexDFA(encoded string, lexstate []int) *jflexDFA {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		panic(err)
	}
	le := binary.LittleEndian
	nTop, nBlocks, nStates, nTrans := int(le.Uint32(data[0:])), int(le.Uint32(data[4:])), int(le.Uint32(data[8:])), int(le.Uint32(data[12:]))
	off := 16
	d := &jflexDFA{lexstate: lexstate}
	d.top = make([]uint16, nTop)
	for i := range d.top {
		d.top[i] = le.Uint16(data[off:])
		off += 2
	}
	d.blocks = data[off : off+nBlocks]
	off += nBlocks
	d.action = data[off : off+nStates]
	off += nStates
	d.rowmap = make([]uint32, nStates)
	for i := range d.rowmap {
		d.rowmap[i] = le.Uint32(data[off:])
		off += 4
	}
	d.trans = make([]uint16, nTrans)
	for i := range d.trans {
		d.trans[i] = le.Uint16(data[off:])
		off += 2
	}
	d.attr = data[off : off+nStates]
	return d
}

var (
	standardDFAOnce, classicDFAOnce, uaxDFAOnce sync.Once
	standardDFA, classicDFA, uaxDFA             *jflexDFA
)

func getStandardDFA() *jflexDFA {
	standardDFAOnce.Do(func() { standardDFA = loadJFlexDFA(dfaStandard, []int{0, 0}) })
	return standardDFA
}

func getClassicDFA() *jflexDFA {
	classicDFAOnce.Do(func() { classicDFA = loadJFlexDFA(dfaClassic, []int{0, 0}) })
	return classicDFA
}

func getUAXDFA() *jflexDFA {
	uaxDFAOnce.Do(func() { uaxDFA = loadJFlexDFA(dfaUAX29URLEmail, []int{0, 0, 1, 1}) })
	return uaxDFA
}

func (d *jflexDFA) class(c rune) int {
	if c < 0 || c > unicode.MaxRune {
		return 0
	}
	return int(d.blocks[int(d.top[c>>8])<<8|int(c&255)])
}

// jflexScanner runs a JFlex DFA the way Lucene's scanners do. bufSize
// limits how far a match may reach from its start (in UTF-16 units) for
// scanners whose buffer does not grow; 0 means unlimited.
type jflexScanner struct {
	d        *jflexDFA
	rt       runeText
	pos      int
	lexState int
	bufSize  int
}

// next returns the action of the next match and its rune range; the action
// is -1 at the end of the input.
func (s *jflexScanner) next() (int, int, int) {
	n := len(s.rt.runes)
	start := s.pos
	if start >= n {
		return -1, start, start
	}
	limit := n
	if s.bufSize > 0 {
		maxU := s.rt.u16[start] + s.bufSize
		limit = start + sort.SearchInts(s.rt.u16[start:], maxU+1) - 1
		if limit > n {
			limit = n
		}
		if limit <= start {
			limit = start + 1
		}
	}
	state := s.d.lexstate[s.lexState]
	action := -1
	marked := start
	if s.d.attr[state]&1 == 1 {
		action = state
	}
	for cur := start; cur < limit; {
		c := s.rt.runes[cur]
		cur++
		next := int(s.d.trans[int(s.d.rowmap[state])+s.d.class(c)]) - 1
		if next < 0 {
			break
		}
		state = next
		if a := s.d.attr[state]; a&1 == 1 {
			action = state
			marked = cur
			if a&8 == 8 {
				break
			}
		}
	}
	if action < 0 || marked == start {
		s.pos = start + 1
		return 0, start, start + 1
	}
	s.pos = marked
	return int(s.d.action[action]), start, marked
}

var standardTokenTypes = []string{"<ALPHANUM>", "<NUM>", "<SOUTHEAST_ASIAN>", "<IDEOGRAPHIC>", "<HIRAGANA>", "<KATAKANA>", "<HANGUL>", "<EMOJI>"}

// standardActions maps StandardTokenizerImpl actions to token types.
var standardActions = map[int]int{2: 1, 3: 0, 4: 7, 5: 2, 6: 6, 7: 3, 8: 5, 9: 4}

type standardTokenizer struct{ maxLen int }

func (t standardTokenizer) tokenize(text string) anStream {
	rt := newRuneText(text)
	sc := &jflexScanner{d: getStandardDFA(), rt: rt, bufSize: t.maxLen}
	var out anStream
	skipped := 0
	for {
		act, st, en := sc.next()
		if act < 0 {
			break
		}
		typ, ok := standardActions[act]
		if !ok {
			continue
		}
		if rt.u16[en]-rt.u16[st] > t.maxLen {
			skipped++
			continue
		}
		out.tokens = append(out.tokens, anToken{term: rt.str(st, en), start: rt.u16[st], end: rt.u16[en], posInc: skipped + 1, posLen: 1, typ: standardTokenTypes[typ]})
		skipped = 0
	}
	out.finalOffset = rt.u16[len(rt.runes)]
	out.finalPosInc = skipped
	return out
}

var classicTokenTypes = []string{"<ALPHANUM>", "<APOSTROPHE>", "<ACRONYM>", "<COMPANY>", "<EMAIL>", "<HOST>", "<NUM>", "<CJ>", "<ACRONYM_DEP>"}

var classicActions = map[int]int{2: 0, 3: 7, 4: 6, 5: 5, 6: 3, 7: 1, 8: 8, 9: 2, 10: 4}

type classicTokenizer struct{ maxLen int }

func (t classicTokenizer) tokenize(text string) anStream {
	rt := newRuneText(text)
	sc := &jflexScanner{d: getClassicDFA(), rt: rt}
	var out anStream
	skipped := 0
	for {
		act, st, en := sc.next()
		if act < 0 {
			break
		}
		typ, ok := classicActions[act]
		if !ok {
			continue
		}
		if rt.u16[en]-rt.u16[st] > t.maxLen {
			skipped++
			continue
		}
		tok := anToken{term: rt.str(st, en), start: rt.u16[st], end: rt.u16[en], posInc: skipped + 1, posLen: 1, typ: classicTokenTypes[typ]}
		if typ == 8 { // ACRONYM_DEP: a host name with a trailing dot
			tok.typ = classicTokenTypes[5]
			tok.term = rt.str(st, en-1)
		}
		out.tokens = append(out.tokens, tok)
		skipped = 0
	}
	out.finalOffset = rt.u16[len(rt.runes)]
	out.finalPosInc = skipped
	return out
}

var uaxTokenTypes = []string{"<ALPHANUM>", "<NUM>", "<SOUTHEAST_ASIAN>", "<IDEOGRAPHIC>", "<HIRAGANA>", "<KATAKANA>", "<HANGUL>", "<URL>", "<EMAIL>", "<EMOJI>"}

type uaxURLEmailTokenizer struct{ maxLen int }

func (t uaxURLEmailTokenizer) tokenize(text string) anStream {
	rt := newRuneText(text)
	sc := &jflexScanner{d: getUAXDFA(), rt: rt, bufSize: t.maxLen}
	var out anStream
	skipped := 0
	for {
		act, st, en := sc.next()
		if act < 0 {
			break
		}
		typ := -1
		switch act {
		case 2:
			typ = 1
		case 3:
			typ = 0
		case 4:
			typ = 9
		case 5:
			typ = 2
		case 6:
			typ = 6
		case 7:
			typ = 3
		case 8:
			typ = 5
		case 9:
			typ = 4
		case 10:
			typ = 8
		case 11:
			typ = 7
		case 12:
			en--
			sc.pos = en
			typ = 7
		case 13:
			typ = 7
		case 14: // a URL-like match to rescan in the AVOID_BAD_URL state
			sc.pos = st
			sc.lexState = 2
			continue
		case 15:
			en = st + 6
			if en > len(rt.runes) {
				en = len(rt.runes)
			}
			sc.pos = en
			typ = 0
		}
		if act >= 2 && act <= 15 && act != 11 {
			sc.lexState = 0
		}
		if typ < 0 {
			continue
		}
		if rt.u16[en]-rt.u16[st] > t.maxLen {
			skipped++
			continue
		}
		out.tokens = append(out.tokens, anToken{term: rt.str(st, en), start: rt.u16[st], end: rt.u16[en], posInc: skipped + 1, posLen: 1, typ: uaxTokenTypes[typ]})
		skipped = 0
	}
	out.finalOffset = rt.u16[len(rt.runes)]
	out.finalPosInc = skipped
	return out
}

// charTokenizer is Lucene's CharTokenizer.
type charTokenizer struct {
	isTokenChar func(rune) bool
	normalize   func(rune) rune
	maxLen      int
}

func (t charTokenizer) tokenize(text string) anStream {
	var out anStream
	off, start, end, length := 0, -1, -1, 0
	var buf []rune
	emit := func() {
		out.tokens = append(out.tokens, anToken{term: string(buf), start: start, end: end, posInc: 1, posLen: 1, typ: "word"})
		buf, length = buf[:0], 0
	}
	for _, r := range text {
		cc := 1
		if r >= 0x10000 {
			cc = 2
		}
		if t.isTokenChar(r) {
			if length == 0 {
				start, end = off, off
			}
			end += cc
			if t.normalize != nil {
				r = t.normalize(r)
			}
			buf = append(buf, r)
			if r >= 0x10000 {
				length += 2
			} else {
				length++
			}
			off += cc
			if length >= t.maxLen {
				emit()
			}
			continue
		}
		if length > 0 {
			emit()
		}
		off += cc
	}
	if length > 0 {
		emit()
	}
	out.finalOffset = off
	return out
}

// javaIsWhitespace is Character.isWhitespace.
func javaIsWhitespace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r >= 0x1C && r <= 0x1F:
		return true
	case r == 0xA0 || r == 0x2007 || r == 0x202F:
		return false
	}
	return unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp)
}

func javaIsPunctuation(r rune) bool {
	return unicode.In(r, unicode.Ps, unicode.Pe, unicode.Po, unicode.Pc, unicode.Pd, unicode.Pi, unicode.Pf)
}

func javaIsSymbol(r rune) bool { return unicode.In(r, unicode.Sc, unicode.Sm, unicode.So, unicode.Sk) }

func javaToLower(r rune) rune { return unicode.ToLower(r) }

func javaToUpper(r rune) rune { return unicode.ToUpper(r) }

type keywordTokenizer struct{}

func (keywordTokenizer) tokenize(text string) anStream {
	n := utf16Len(text)
	return anStream{tokens: []anToken{{term: text, start: 0, end: n, posInc: 1, posLen: 1, typ: "word"}}, finalOffset: n}
}

// ngramTokenizer is Lucene's NGramTokenizer (edgesOnly: EdgeNGramTokenizer).
type ngramTokenizer struct {
	minGram, maxGram int
	edgesOnly        bool
	isTokenChar      func(rune) bool // nil: every character
}

func (t ngramTokenizer) tokenize(text string) anStream {
	rt := newRuneText(text)
	buf := rt.runes
	n := len(buf)
	var out anStream
	bufferStart, gramSize := 0, t.minGram
	lastNonTokenChar, lastCheckedChar := -1, -1
	isTok := func(r rune) bool { return t.isTokenChar == nil || t.isTokenChar(r) }
	for {
		if gramSize > t.maxGram || bufferStart+gramSize > n {
			if bufferStart+1+t.minGram > n {
				break
			}
			bufferStart++
			gramSize = t.minGram
		}
		termEnd := bufferStart + gramSize - 1
		if termEnd > lastCheckedChar {
			for i := termEnd; i > lastCheckedChar; i-- {
				if !isTok(buf[i]) {
					lastNonTokenChar = i
					break
				}
			}
			lastCheckedChar = termEnd
		}
		if (lastNonTokenChar >= bufferStart && lastNonTokenChar < bufferStart+gramSize) || (t.edgesOnly && lastNonTokenChar != bufferStart-1) {
			bufferStart++
			gramSize = t.minGram
			if bufferStart >= n {
				break
			}
			continue
		}
		out.tokens = append(out.tokens, anToken{term: rt.str(bufferStart, bufferStart+gramSize), start: rt.u16[bufferStart], end: rt.u16[bufferStart+gramSize], posInc: 1, posLen: 1, typ: "word"})
		gramSize++
	}
	out.finalOffset = rt.u16[n]
	return out
}

// utf16Positions maps byte offsets of s to UTF-16 offsets.
func utf16Positions(s string) func(int) int {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return func(b int) int { return b }
	}
	table := make([]int, len(s)+1)
	u := 0
	for i, r := range s {
		size := utf8.RuneLen(r)
		if size < 0 {
			size = 1
		}
		for k := 0; k < size && i+k < len(s); k++ {
			table[i+k] = u
		}
		if r >= 0x10000 {
			u += 2
		} else {
			u++
		}
	}
	table[len(s)] = u
	return func(b int) int { return table[b] }
}

// patternTokenizer is Lucene's PatternTokenizer.
type patternTokenizer struct {
	re    *regexp.Regexp
	group int
}

func (t patternTokenizer) tokenize(text string) anStream {
	u := utf16Positions(text)
	var out anStream
	add := func(i, j int) {
		out.tokens = append(out.tokens, anToken{term: text[i:j], start: u(i), end: u(j), posInc: 1, posLen: 1, typ: "word"})
	}
	if t.group >= 0 {
		for _, m := range t.re.FindAllStringSubmatchIndex(text, -1) {
			if 2*t.group+1 < len(m) {
				i, j := m[2*t.group], m[2*t.group+1]
				if i >= 0 && j > i {
					add(i, j)
				}
			}
		}
	} else {
		index := 0
		for _, m := range t.re.FindAllStringIndex(text, -1) {
			if m[0]-index > 0 {
				add(index, m[0])
			}
			index = m[1]
		}
		if len(text)-index > 0 {
			add(index, len(text))
		}
	}
	out.finalOffset = u(len(text))
	return out
}

// simplePatternTokenizer is SimplePatternTokenizer (split=false) or
// SimplePatternSplitTokenizer (split=true).
type simplePatternTokenizer struct {
	re    *regexp.Regexp
	split bool
}

func (t simplePatternTokenizer) tokenize(text string) anStream {
	u := utf16Positions(text)
	var out anStream
	add := func(i, j int) {
		if j > i {
			out.tokens = append(out.tokens, anToken{term: text[i:j], start: u(i), end: u(j), posInc: 1, posLen: 1, typ: "word"})
		}
	}
	prev := 0
	for _, m := range t.re.FindAllStringIndex(text, -1) {
		if m[1] == m[0] {
			continue
		}
		if t.split {
			add(prev, m[0])
			prev = m[1]
		} else {
			add(m[0], m[1])
		}
	}
	if t.split {
		add(prev, len(text))
	}
	out.finalOffset = u(len(text))
	return out
}

// pathHierarchyTokenizer is PathHierarchyTokenizer and
// ReversePathHierarchyTokenizer (positions increment, as in Lucene 10).
type pathHierarchyTokenizer struct {
	delimiter, replacement uint16
	skip                   int
	reverse                bool
}

func (t pathHierarchyTokenizer) tokenize(text string) anStream {
	in := utf16.Encode([]rune(text))
	var out anStream
	emit := func(term []uint16, start, end int) {
		out.tokens = append(out.tokens, anToken{term: string(utf16.Decode(term)), start: start, end: end, posInc: 1, posLen: 1, typ: "word"})
	}
	out.finalOffset = len(in)
	if t.reverse {
		positions := []int{0}
		result := make([]uint16, 0, len(in))
		for i, c := range in {
			if c == t.delimiter {
				positions = append(positions, i+1)
				result = append(result, t.replacement)
			} else {
				result = append(result, c)
			}
		}
		if positions[len(positions)-1] < len(in) {
			positions = append(positions, len(in))
		}
		idx := len(positions) - 1 - t.skip
		if idx < 0 {
			return out
		}
		endPos := positions[idx]
		for k := 0; k < len(positions)-t.skip-1; k++ {
			start := positions[k]
			emit(result[start:endPos], start, endPos)
		}
		return out
	}
	var resultToken []uint16
	startPosition, skipped, pos := 0, 0, 0
	endDelimiter := false
	for {
		term := append([]uint16(nil), resultToken...)
		length := 0
		added := false
		if endDelimiter {
			term = append(term, t.replacement)
			length++
			endDelimiter = false
			added = true
		}
		done := false
		for {
			if pos >= len(in) {
				if skipped > t.skip {
					length += len(resultToken)
					if added {
						emit(term[:length], startPosition, startPosition+length)
						resultToken = term[:length]
					}
				}
				done = true
				break
			}
			c := in[pos]
			pos++
			if !added {
				added = true
				skipped++
				if skipped > t.skip {
					if c == t.delimiter {
						c = t.replacement
					}
					term = append(term, c)
					length++
				} else {
					startPosition++
				}
			} else if c == t.delimiter {
				if skipped > t.skip {
					endDelimiter = true
					break
				}
				skipped++
				if skipped > t.skip {
					term = append(term, t.replacement)
					length++
				} else {
					startPosition++
				}
			} else if skipped > t.skip {
				term = append(term, c)
				length++
			} else {
				startPosition++
			}
		}
		if done {
			break
		}
		length += len(resultToken)
		emit(term[:length], startPosition, startPosition+length)
		resultToken = term[:length]
	}
	return out
}

// ---------------------------------------------------------------------------
// token filters
// ---------------------------------------------------------------------------

// tokenFunc adapts a function over the whole stream.
type tokenFunc func(anStream) anStream

func (f tokenFunc) apply(in anStream) anStream { return f(in) }

// mapTerms changes each token in place.
func mapTerms(f func(t *anToken)) tokenFunc {
	return func(in anStream) anStream {
		for i := range in.tokens {
			f(&in.tokens[i])
		}
		return in
	}
}

// filtering is Lucene's FilteringTokenFilter: removed tokens pass their
// position increment on to the next kept token.
func filtering(accept func(t *anToken) bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset}
		skipped := 0
		for _, t := range in.tokens {
			if accept(&t) {
				t.posInc += skipped
				skipped = 0
				out.tokens = append(out.tokens, t)
			} else {
				skipped += t.posInc
			}
		}
		out.finalPosInc = in.finalPosInc + skipped
		return out
	}
}

// wordSet is Lucene's CharArraySet.
type wordSet struct {
	words      map[string]bool
	ignoreCase bool
}

func newWordSet(words []string, ignoreCase bool) *wordSet {
	s := &wordSet{words: make(map[string]bool, len(words)), ignoreCase: ignoreCase}
	for _, w := range words {
		s.add(w)
	}
	return s
}

func (s *wordSet) add(w string) {
	if s.ignoreCase {
		w = strings.Map(javaToLower, w)
	}
	s.words[w] = true
}

func (s *wordSet) contains(w string) bool {
	if s == nil {
		return false
	}
	if s.ignoreCase {
		w = strings.Map(javaToLower, w)
	}
	return s.words[w]
}

func lowercaseFilter(t *anToken) { t.term = strings.Map(javaToLower, t.term) }

func uppercaseFilter(t *anToken) { t.term = strings.Map(javaToUpper, t.term) }

func greekLowercase(r rune) rune {
	switch r {
	case 0x03C2:
		return 0x03C3
	case 0x0386, 0x03AC:
		return 0x03B1
	case 0x0388, 0x03AD:
		return 0x03B5
	case 0x0389, 0x03AE:
		return 0x03B7
	case 0x038A, 0x03AA, 0x03AF, 0x03CA, 0x0390:
		return 0x03B9
	case 0x038E, 0x03AB, 0x03CD, 0x03CB, 0x03B0:
		return 0x03C5
	case 0x038C, 0x03CC:
		return 0x03BF
	case 0x038F, 0x03CE:
		return 0x03C9
	case 0x03A2:
		return 0x03C2
	}
	return javaToLower(r)
}

func turkishLowercaseFilter(t *anToken) {
	rs := []rune(t.term)
	out := make([]rune, 0, len(rs))
	iOrAfter := false
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		iOrAfter = ch == 'I' || (iOrAfter && unicode.Is(unicode.Mn, ch))
		if iOrAfter {
			if ch == 0x0307 {
				continue
			}
			if ch == 'I' {
				dot := false
				for k := i + 1; k < len(rs); k++ {
					if rs[k] == 0x0307 {
						dot = true
						break
					}
					if !unicode.Is(unicode.Mn, rs[k]) {
						break
					}
				}
				if dot {
					out = append(out, 'i')
				} else {
					out = append(out, 0x0131)
					iOrAfter = false
				}
				continue
			}
		}
		out = append(out, javaToLower(ch))
	}
	t.term = string(out)
}

func irishLowercaseFilter(t *anToken) {
	rs := []rune(t.term)
	upperVowel := func(r rune) bool { return strings.ContainsRune("AEIOUÁÉÍÓÚ", r) }
	prefix := ""
	if len(rs) > 1 && (rs[0] == 'n' || rs[0] == 't') && upperVowel(rs[1]) {
		prefix = string(rs[0]) + "-"
		rs = rs[1:]
	}
	t.term = prefix + strings.Map(javaToLower, string(rs))
}

func asciiFoldingChainFilter(preserveOriginal bool) tokenFunc {
	folder := bleveasciifolding.New()
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc, tokens: make([]anToken, 0, len(in.tokens))}
		for _, t := range in.tokens {
			folded := t.term
			for i := 0; i < len(t.term); i++ {
				if t.term[i] >= 0x80 {
					folded = string(folder.Filter([]byte(t.term)))
					break
				}
			}
			orig := t
			t.term = folded
			out.tokens = append(out.tokens, t)
			if preserveOriginal && folded != orig.term {
				orig.posInc = 0
				out.tokens = append(out.tokens, orig)
			}
		}
		return out
	}
}

func stopTokenFilter(words *wordSet, removeTrailing bool) tokenFunc {
	if removeTrailing {
		return filtering(func(t *anToken) bool { return !words.contains(t.term) })
	}
	// SuggestStopFilter: a trailing stop word without a separator after it
	// is kept (as a keyword)
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		skipped := 0
		for i, t := range in.tokens {
			if words.contains(t.term) {
				if i+1 < len(in.tokens) {
					skipped += t.posInc
					continue
				}
				if in.finalOffset > t.end {
					return out
				}
				t.keyword = true
			}
			t.posInc += skipped
			skipped = 0
			out.tokens = append(out.tokens, t)
		}
		return out
	}
}

func javaTrimWhitespace(s string) string { return strings.TrimFunc(s, javaIsWhitespace) }

// truncateRunes cuts a term to at most n UTF-16 units.
func truncateUTF16(s string, n int) string {
	u := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if u+w > n {
			return s[:i]
		}
		u += w
	}
	return s
}

func uniqueTokenFilter(onlyOnSamePosition bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		seen := map[string]bool{}
		for _, t := range in.tokens {
			dup := false
			if onlyOnSamePosition {
				if t.posInc > 0 {
					seen = map[string]bool{}
				}
				dup = t.posInc == 0 && seen[t.term]
			} else {
				dup = seen[t.term]
			}
			seen[t.term] = true
			if !dup {
				out.tokens = append(out.tokens, t)
			}
		}
		return out
	}
}

func reverseRunes(s string) string {
	rs := []rune(s)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return string(rs)
}

func elisionFilter(articles *wordSet) func(t *anToken) {
	return func(t *anToken) {
		if i := strings.IndexAny(t.term, "'’"); i >= 0 && articles.contains(t.term[:i]) {
			_, size := utf8.DecodeRuneInString(t.term[i:])
			t.term = t.term[i+size:]
		}
	}
}

func decimalDigitFilter(t *anToken) {
	t.term = strings.Map(func(r rune) rune {
		if r < 0x80 || !unicode.Is(unicode.Nd, r) {
			return r
		}
		for _, rg := range unicode.Nd.R16 {
			if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
				return '0' + (r-rune(rg.Lo))%10
			}
		}
		for _, rg := range unicode.Nd.R32 {
			if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
				return '0' + (r-rune(rg.Lo))%10
			}
		}
		return r
	}, t.term)
}

func cjkWidthFilter() func(t *anToken) {
	f := blevecjk.NewCJKWidthFilter()
	return func(t *anToken) {
		ts := f.Filter(analysis.TokenStream{&analysis.Token{Term: []byte(t.term)}})
		if len(ts) == 1 {
			t.term = string(ts[0].Term)
		}
	}
}

var keywordRepeatFilter tokenFunc = func(in anStream) anStream {
	out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc, tokens: make([]anToken, 0, 2*len(in.tokens))}
	for _, t := range in.tokens {
		k := t
		k.keyword = true
		out.tokens = append(out.tokens, k)
		t.posInc = 0
		t.keyword = false
		out.tokens = append(out.tokens, t)
	}
	return out
}

var removeDuplicatesFilter = uniqueTokenFilter(true)

func apostropheFilter(t *anToken) {
	if i := strings.IndexAny(t.term, "'’"); i >= 0 {
		t.term = t.term[:i]
	}
}

func classicFilter(t *anToken) {
	switch t.typ {
	case "<APOSTROPHE>":
		if n := len(t.term); n >= 2 && t.term[n-2] == '\'' && (t.term[n-1] == 's' || t.term[n-1] == 'S') {
			t.term = t.term[:n-2]
		}
	case "<ACRONYM>":
		t.term = strings.ReplaceAll(t.term, ".", "")
	}
}

func fingerprintFilter(separator string, maxOutput int) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		seen := map[string]bool{}
		var uniq []string
		size := 0
		for _, t := range in.tokens {
			if size > maxOutput {
				continue
			}
			if !seen[t.term] {
				seen[t.term] = true
				if len(uniq) > 0 {
					size++
				}
				uniq = append(uniq, t.term)
				size += utf16Len(t.term)
			}
		}
		if len(uniq) == 0 || size > maxOutput {
			return out
		}
		sort.Slice(uniq, func(i, j int) bool { return compareUTF16(uniq[i], uniq[j]) < 0 })
		out.tokens = []anToken{{term: strings.Join(uniq, separator), start: 0, end: in.finalOffset, posInc: 1, posLen: 1, typ: "fingerprint"}}
		return out
	}
}

// compareUTF16 orders strings by UTF-16 code units, like Java's String.
func compareUTF16(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return int(ua[i]) - int(ub[i])
		}
	}
	return len(ua) - len(ub)
}

func limitFilter(max int) tokenFunc {
	return func(in anStream) anStream {
		if len(in.tokens) > max {
			in.tokens = in.tokens[:max]
		}
		return in
	}
}

func delimitedPayloadFilter(delim rune, encoding string) func(t *anToken) {
	return func(t *anToken) {
		i := strings.IndexRune(t.term, delim)
		if i < 0 {
			t.payload = nil
			return
		}
		value := t.term[i+utf8.RuneLen(delim):]
		t.term = t.term[:i]
		switch encoding {
		case "float":
			f, _ := strconv.ParseFloat(value, 32)
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, math.Float32bits(float32(f)))
			t.payload = b
		case "int":
			n, _ := strconv.ParseInt(value, 10, 32)
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, uint32(int32(n)))
			t.payload = b
		default:
			t.payload = []byte(value)
		}
	}
}

func commonGramsFilter(words *wordSet, queryMode bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		var prev *anToken
		for i := range in.tokens {
			t := in.tokens[i]
			common := words.contains(t.term)
			if prev != nil && (words.contains(prev.term) || common) {
				out.tokens = append(out.tokens, anToken{term: prev.term + "_" + t.term, start: prev.start, end: t.end, posInc: 0, posLen: 2, typ: "gram"})
			}
			out.tokens = append(out.tokens, t)
			prev = &in.tokens[i]
		}
		if !queryMode {
			return out
		}
		// CommonGramsQueryFilter: drop unigrams covered by a following gram
		var q []anToken
		for i, t := range out.tokens {
			next := i + 1
			if t.typ != "gram" && next < len(out.tokens) && out.tokens[next].typ == "gram" {
				continue
			}
			if t.typ != "gram" && len(q) > 0 && q[len(q)-1].typ == "gram" && i > 0 && out.tokens[i-1].typ == "gram" {
				continue
			}
			if t.typ == "gram" {
				t.posInc = 1
				t.posLen = 1
			}
			q = append(q, t)
		}
		out.tokens = q
		return out
	}
}

// stemmers skip tokens marked as keywords
func stemFilter(stem func(string) string) func(t *anToken) {
	return func(t *anToken) {
		if !t.keyword {
			t.term = stem(t.term)
		}
	}
}

func porterStem(s string) string { return string(porterstemmer.StemWithoutLowerCasing([]rune(s))) }

// snowballStemmers are the Snowball stemmers available to osmem, by
// lower-cased language name.
var snowballStemmers = map[string]func(*snowballstem.Env) bool{
	"danish": danish.Stem, "dutch": dutch.Stem, "english": english.Stem, "porter2": english.Stem, "finnish": finnish.Stem, "french": french.Stem,
	"german": german.Stem, "german2": german.Stem, "hungarian": hungarian.Stem, "irish": irish.Stem, "italian": italian.Stem,
	"norwegian": norwegian.Stem, "porter": porter.Stem, "portuguese": portuguese.Stem, "romanian": romanian.Stem,
	"russian": russian.Stem, "spanish": spanish.Stem, "swedish": swedish.Stem, "turkish": turkish.Stem,
}

func snowballStem(fn func(*snowballstem.Env) bool) func(string) string {
	return func(s string) string {
		env := snowballstem.NewEnv(s)
		fn(env)
		return env.Current()
	}
}

func englishPossessive(s string) string {
	rs := []rune(s)
	n := len(rs)
	if n >= 2 && (rs[n-2] == '\'' || rs[n-2] == 0x2019 || rs[n-2] == 0xFF07) && (rs[n-1] == 's' || rs[n-1] == 'S') {
		return string(rs[:n-2])
	}
	return s
}

func englishMinimalStem(s string) string {
	rs := []rune(s)
	n := len(rs)
	if n < 3 || rs[n-1] != 's' {
		return s
	}
	switch rs[n-2] {
	case 'u', 's':
		return s
	case 'e':
		if n > 3 && rs[n-3] == 'i' && rs[n-4] != 'a' && rs[n-4] != 'e' {
			rs[n-3] = 'y'
			return string(rs[:n-2])
		}
		if rs[n-3] == 'i' || rs[n-3] == 'a' || rs[n-3] == 'o' || rs[n-3] == 'e' {
			return s
		}
	}
	return string(rs[:n-1])
}

// bleveTermFilter runs a one-token-at-a-time bleve filter on a term.
func bleveTermFilter(f analysis.TokenFilter) func(string) string {
	return func(s string) string {
		ts := f.Filter(analysis.TokenStream{&analysis.Token{Term: []byte(s)}})
		if len(ts) != 1 {
			return s
		}
		return string(ts[0].Term)
	}
}

// stemmerForLanguage returns the stemmer of the stemmer token filter; ok is
// false for a known language osmem cannot emulate.
func stemmerForLanguage(language string) (func(string) string, bool, error) {
	switch l := strings.ToLower(language); l {
	case "english", "porter":
		return porterStem, true, nil
	case "possessive_english", "possessiveenglish":
		return englishPossessive, true, nil
	case "minimal_english", "minimalenglish":
		return englishMinimalStem, true, nil
	case "light_french", "lightfrench":
		return bleveTermFilter(blevefr.NewFrenchLightStemmerFilter()), true, nil
	case "minimal_french", "minimalfrench":
		return bleveTermFilter(blevefr.NewFrenchMinimalStemmerFilter()), true, nil
	case "light_german", "lightgerman":
		return bleveTermFilter(blevede.NewGermanLightStemmerFilter()), true, nil
	case "light_spanish", "lightspanish":
		return spanishLightStem, true, nil
	case "light_italian", "lightitalian":
		return bleveTermFilter(bleveit.NewItalianLightStemmerFilterFilter()), true, nil
	case "light_portuguese", "lightportuguese":
		return bleveTermFilter(blevept.NewPortugueseLightStemmerFilter()), true, nil
	case "arabic", "armenian", "basque", "bengali", "brazilian", "bulgarian", "catalan", "czech", "dutch", "dutch_kp", "dutchkp", "kp",
		"light_english", "lightenglish", "kstem", "estonian", "light_finish", "lightfinish", "light_finnish", "lightfinnish",
		"galician", "minimal_galician", "minimal_german", "minimalgerman", "greek", "hindi", "light_hungarian", "lighthungarian",
		"indonesian", "latvian", "lithuanian", "light_norwegian", "lightnorwegian", "minimal_norwegian", "minimalnorwegian",
		"light_nynorsk", "lightnynorsk", "minimal_nynorsk", "minimalnynorsk", "minimal_portuguese", "minimalportuguese",
		"portuguese_rslp", "light_russian", "lightrussian", "sorani", "light_swedish", "lightswedish":
		return nil, false, nil
	default:
		if fn, ok := snowballStemmers[l]; ok {
			return snowballStem(fn), true, nil
		}
		return nil, false, invalidStemmerError(language)
	}
}

func invalidStemmerError(language string) error {
	name := language
	if name != "" {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Invalid stemmer class specified: " + name,
		Cause: &Error{Type: "class_not_found_exception", Reason: "org.tartarus.snowball.ext." + name + "Stemmer"}}
}

// snowballForLanguage is the snowball token filter's stemmer.
func snowballForLanguage(language string) (func(string) string, bool, error) {
	l := strings.ToLower(language)
	if fn, ok := snowballStemmers[l]; ok && l != "porter2" {
		return snowballStem(fn), true, nil
	}
	switch l {
	case "armenian", "basque", "catalan", "dutch", "estonian", "kp", "lithuanian":
		return nil, false, nil
	}
	return nil, false, invalidStemmerError(language)
}

func keywordMarkerFilter(words *wordSet, pattern *regexp.Regexp) func(t *anToken) {
	return func(t *anToken) {
		if pattern != nil {
			if loc := pattern.FindStringIndex(t.term); loc != nil && loc[0] == 0 && loc[1] == len(t.term) {
				t.keyword = true
			}
		} else if words.contains(t.term) {
			t.keyword = true
		}
	}
}

func stemmerOverrideFilter(rules map[string]string) func(t *anToken) {
	return func(t *anToken) {
		if t.keyword {
			return
		}
		if v, ok := rules[t.term]; ok {
			t.term = v
			t.keyword = true
		}
	}
}

func patternReplaceTokenFilter(re *regexp.Regexp, replacement string, all bool) func(t *anToken) {
	return func(t *anToken) {
		var sb strings.Builder
		last := 0
		for _, m := range re.FindAllStringSubmatchIndex(t.term, -1) {
			sb.WriteString(t.term[last:m[0]])
			sb.WriteString(javaReplacement(replacement, t.term, m))
			last = m[1]
			if !all {
				break
			}
		}
		sb.WriteString(t.term[last:])
		t.term = sb.String()
	}
}

// patternCaptureFilter is PatternCaptureGroupTokenFilter.
func patternCaptureFilter(res []*regexp.Regexp, preserveOriginal bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		for _, t := range in.tokens {
			term := t.term
			type capture struct{ start, end int }
			var caps []capture
			// all non-empty groups of all matches, ordered by start (ties keep
			// pattern order), as nextCapture visits them
			type pending struct {
				matches [][]int
				m, g    int
			}
			ps := make([]*pending, len(res))
			for i, re := range res {
				ps[i] = &pending{matches: re.FindAllStringSubmatchIndex(term, -1), g: 1}
			}
			for {
				best, bestStart := -1, math.MaxInt
				for i, p := range ps {
					for p.m < len(p.matches) {
						match := p.matches[p.m]
						if p.g >= len(match)/2 {
							p.m++
							p.g = 1
							continue
						}
						s, e := match[2*p.g], match[2*p.g+1]
						if s < 0 || s == e || (preserveOriginal && s == 0 && e == len(term)) {
							p.g++
							continue
						}
						if s < bestStart {
							best, bestStart = i, s
						}
						break
					}
				}
				if best < 0 {
					break
				}
				p := ps[best]
				match := p.matches[p.m]
				caps = append(caps, capture{match[2*p.g], match[2*p.g+1]})
				p.g++
			}
			if preserveOriginal || len(caps) == 0 {
				out.tokens = append(out.tokens, t)
				for _, c := range caps {
					k := t
					k.term = term[c.start:c.end]
					k.posInc = 0
					out.tokens = append(out.tokens, k)
				}
				continue
			}
			for i, c := range caps {
				k := t
				k.term = term[c.start:c.end]
				if i > 0 {
					k.posInc = 0
				}
				out.tokens = append(out.tokens, k)
			}
		}
		return out
	}
}

func keepWordsFilter(words *wordSet) tokenFunc {
	return filtering(func(t *anToken) bool { return words.contains(t.term) })
}

func keepTypesFilter(types map[string]bool, include bool) tokenFunc {
	return filtering(func(t *anToken) bool { return types[t.typ] == include })
}

// dictionaryDecompounder is DictionaryCompoundWordTokenFilter.
func dictionaryDecompounder(dict *wordSet, minWord, minSub, maxSub int, onlyLongest bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		for _, t := range in.tokens {
			out.tokens = append(out.tokens, t)
			if t.keyword {
				continue
			}
			u := utf16.Encode([]rune(t.term))
			if len(u) < minWord {
				continue
			}
			for i := 0; i <= len(u)-minSub; i++ {
				longest := -1
				for j := minSub; j <= maxSub && i+j <= len(u); j++ {
					if dict.contains(string(utf16.Decode(u[i : i+j]))) {
						if onlyLongest {
							longest = j
						} else {
							k := t
							k.term = string(utf16.Decode(u[i : i+j]))
							k.posInc = 0
							out.tokens = append(out.tokens, k)
						}
					}
				}
				if onlyLongest && longest > 0 {
					k := t
					k.term = string(utf16.Decode(u[i : i+longest]))
					k.posInc = 0
					out.tokens = append(out.tokens, k)
				}
			}
		}
		return out
	}
}

// ngramTokenFilter is NGramTokenFilter.
func ngramTokenFilter(minGram, maxGram int, preserveOriginal bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset}
		posInc := 0
		for _, t := range in.tokens {
			rs := []rune(t.term)
			posInc += t.posInc
			if len(rs) < minGram {
				if preserveOriginal {
					t.posInc = posInc
					posInc = 0
					out.tokens = append(out.tokens, t)
				}
				continue
			}
			for pos := 0; pos+minGram <= len(rs); pos++ {
				for g := minGram; g <= maxGram && pos+g <= len(rs); g++ {
					k := t
					k.term = string(rs[pos : pos+g])
					k.posInc = posInc
					posInc = 0
					out.tokens = append(out.tokens, k)
				}
			}
			if preserveOriginal && len(rs) > maxGram {
				t.posInc = 0
				out.tokens = append(out.tokens, t)
			}
		}
		out.finalPosInc = in.finalPosInc + posInc
		return out
	}
}

// edgeNGramTokenFilter is EdgeNGramTokenFilter (back: OpenSearch's
// side=back, a reversed edge n-gram).
func edgeNGramTokenFilter(minGram, maxGram int, preserveOriginal, back bool) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset}
		posInc := 0
		for _, t := range in.tokens {
			rs := []rune(t.term)
			if back {
				rs = []rune(reverseRunes(t.term))
			}
			posInc += t.posInc
			if len(rs) < minGram {
				if preserveOriginal {
					t.posInc = posInc
					posInc = 0
					out.tokens = append(out.tokens, t)
				}
				continue
			}
			for g := minGram; g <= maxGram && g <= len(rs); g++ {
				k := t
				k.term = string(rs[:g])
				if back {
					k.term = reverseRunes(k.term)
				}
				k.posInc = posInc
				posInc = 0
				out.tokens = append(out.tokens, k)
			}
			if preserveOriginal && len(rs) > maxGram {
				t.posInc = 0
				out.tokens = append(out.tokens, t)
			}
		}
		out.finalPosInc = in.finalPosInc + posInc
		return out
	}
}

// shingleFilter is ShingleFilter.
type shingleFilter struct {
	min, max             int
	outputUnigrams       bool
	unigramsIfNoShingles bool
	separator, filler    string
}

func (f shingleFilter) apply(in anStream) anStream {
	type wtok struct {
		anToken
		filler bool
	}
	var window []wtok
	for _, t := range in.tokens {
		if t.posInc > 1 {
			n := t.posInc - 1
			if n > f.max-1 {
				n = f.max - 1
			}
			for i := 0; i < n; i++ {
				window = append(window, wtok{anToken: anToken{term: f.filler, start: t.start, end: t.start, typ: t.typ}, filler: true})
			}
		}
		window = append(window, wtok{anToken: t})
	}
	if n := in.finalPosInc; n > 0 && len(window) > 0 {
		if n > f.max-1 {
			n = f.max - 1
		}
		for i := 0; i < n; i++ {
			window = append(window, wtok{anToken: anToken{term: f.filler, start: in.finalOffset, end: in.finalOffset, typ: "word"}, filler: true})
		}
	}
	out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
	shingles := false
	var sizes []int
	if f.outputUnigrams {
		sizes = append(sizes, 1)
	}
	for size := f.min; size <= f.max; size++ {
		sizes = append(sizes, size)
	}
	for i := range window {
		first := true
		for _, size := range sizes {
			if i+size > len(window) {
				break
			}
			allFiller := true
			parts := make([]string, 0, size)
			for _, w := range window[i : i+size] {
				parts = append(parts, w.term)
				if !w.filler {
					allFiller = false
				}
			}
			if allFiller {
				continue
			}
			tok := window[i].anToken
			tok.term = strings.Join(parts, f.separator)
			tok.end = window[i+size-1].end
			tok.posInc = 0
			if first {
				tok.posInc = 1
			}
			first = false
			if size > 1 {
				tok.typ = "shingle"
				shingles = true
			}
			if f.outputUnigrams {
				tok.posLen = size
			} else if tok.posLen = size - f.min + 1; tok.posLen < 1 {
				tok.posLen = 1
			}
			out.tokens = append(out.tokens, tok)
		}
	}
	if !f.outputUnigrams && !shingles && f.unigramsIfNoShingles {
		out.tokens = append([]anToken(nil), in.tokens...)
	}
	return out
}

// cjkBigramFilter is CJKBigramFilter.
type cjkBigramFilter struct {
	han, hiragana, katakana, hangul bool
	outputUnigrams                  bool
}

func (f cjkBigramFilter) apply(in anStream) anStream {
	out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
	var buf []rune
	var starts, ends []int
	index := 0
	lastEnd := -1
	ngramState := false
	cjkType := func(typ string) bool {
		switch typ {
		case "<IDEOGRAPHIC>":
			return f.han
		case "<HIRAGANA>":
			return f.hiragana
		case "<KATAKANA>":
			return f.katakana
		case "<HANGUL>":
			return f.hangul
		}
		return false
	}
	hasBigram := func() bool { return len(buf)-index > 1 }
	hasUnigram := func() bool {
		if f.outputUnigrams {
			return len(buf)-index == 1
		}
		return len(buf) == 1 && index == 0
	}
	flushBigram := func() {
		t := anToken{term: string(buf[index : index+2]), start: starts[index], end: ends[index+1], posInc: 1, posLen: 1, typ: "<DOUBLE>"}
		if f.outputUnigrams {
			t.posInc, t.posLen = 0, 2
		}
		out.tokens = append(out.tokens, t)
		index++
	}
	flushUnigram := func() {
		out.tokens = append(out.tokens, anToken{term: string(buf[index]), start: starts[index], end: ends[index], posInc: 1, posLen: 1, typ: "<SINGLE>"})
		index++
	}
	refill := func(t anToken) {
		rs := []rune(t.term)
		lastEnd = t.end
		if t.end-t.start != utf16Len(t.term) {
			for _, r := range rs {
				buf = append(buf, r)
				starts = append(starts, t.start)
				ends = append(ends, t.end)
			}
			return
		}
		s := t.start
		for _, r := range rs {
			w := 1
			if r >= 0x10000 {
				w = 2
			}
			buf = append(buf, r)
			starts = append(starts, s)
			s += w
			ends = append(ends, s)
		}
	}
	i := 0
	var lone *anToken
	next := func() (anToken, bool) {
		if lone != nil {
			t := *lone
			lone = nil
			return t, true
		}
		if i < len(in.tokens) {
			i++
			return in.tokens[i-1], true
		}
		return anToken{}, false
	}
	for {
		if hasBigram() {
			if f.outputUnigrams {
				if ngramState {
					flushBigram()
				} else {
					flushUnigram()
					index--
				}
				ngramState = !ngramState
			} else {
				flushBigram()
			}
			continue
		}
		t, ok := next()
		if !ok {
			if hasUnigram() {
				flushUnigram()
				continue
			}
			break
		}
		if cjkType(t.typ) {
			if t.start != lastEnd {
				if hasUnigram() {
					tt := t
					lone = &tt
					flushUnigram()
					continue
				}
				buf, starts, ends, index = buf[:0], starts[:0], ends[:0], 0
			}
			refill(t)
			continue
		}
		if hasUnigram() {
			tt := t
			lone = &tt
			flushUnigram()
			continue
		}
		out.tokens = append(out.tokens, t)
	}
	return out
}

// word delimiter character types (WordDelimiterIterator)
const (
	wdLower        = 0x01
	wdUpper        = 0x02
	wdDigit        = 0x04
	wdSubwordDelim = 0x08
	wdAlpha        = wdLower | wdUpper
	wdAlphaNum     = wdAlpha | wdDigit
)

// wdCharType is WordDelimiterIterator.getType.
func wdCharType(c uint16) byte {
	r := rune(c)
	switch {
	case c >= 0xD800 && c <= 0xDFFF:
		return wdAlpha | wdDigit
	case unicode.Is(unicode.Lu, r):
		return wdUpper
	case unicode.Is(unicode.Ll, r):
		return wdLower
	case unicode.In(r, unicode.Lt, unicode.Lm, unicode.Lo, unicode.Mn, unicode.Me, unicode.Mc):
		return wdAlpha
	case unicode.In(r, unicode.Nd, unicode.Nl, unicode.No):
		return wdDigit
	}
	return wdSubwordDelim
}

// wdDefaultType is DEFAULT_WORD_DELIM_TABLE for Latin-1 and getType beyond.
func wdDefaultType(c uint16) byte {
	if c < 256 {
		r := rune(c)
		switch {
		case unicode.IsLower(r) || c == 0xAA || c == 0xBA:
			return wdLower
		case unicode.IsUpper(r):
			return wdUpper
		case unicode.Is(unicode.Nd, r):
			return wdDigit
		}
		return wdSubwordDelim
	}
	return wdCharType(c)
}

type wordDelimiterOptions struct {
	generateWordParts, generateNumberParts, catenateWords, catenateNumbers, catenateAll bool
	splitOnCaseChange, preserveOriginal, splitOnNumerics, stemEnglishPossessive         bool
	adjustOffsets, ignoreKeywords                                                       bool
	protected                                                                           *wordSet
	types                                                                               map[uint16]byte // type_table overrides; nil: default table
}

func (o *wordDelimiterOptions) charType(c uint16) byte {
	if o.types != nil {
		if t, ok := o.types[c]; ok {
			return t
		}
		return wdCharType(c)
	}
	return wdDefaultType(c)
}

// wdIterator is WordDelimiterIterator.
type wdIterator struct {
	o                                    *wordDelimiterOptions
	text                                 []uint16
	startBounds, endBounds, current, end int
	skipPossessive, hasFinalPossessive   bool
}

const wdDone = -1

func newWDIterator(o *wordDelimiterOptions, text []uint16) *wdIterator {
	it := &wdIterator{o: o, text: text, endBounds: len(text)}
	for it.startBounds < len(text) && it.o.charType(text[it.startBounds])&wdSubwordDelim != 0 {
		it.startBounds++
	}
	for it.endBounds > it.startBounds && it.o.charType(text[it.endBounds-1])&wdSubwordDelim != 0 {
		it.endBounds--
	}
	if it.endsWithPossessive(it.endBounds) {
		it.hasFinalPossessive = true
	}
	it.current = it.startBounds
	return it
}

func (it *wdIterator) endsWithPossessive(pos int) bool {
	t := it.text
	return it.o.stemEnglishPossessive && pos > 2 && t[pos-2] == '\'' && (t[pos-1] == 's' || t[pos-1] == 'S') &&
		it.o.charType(t[pos-3])&wdAlpha != 0 && (pos == it.endBounds || it.o.charType(t[pos])&wdSubwordDelim != 0)
}

func (it *wdIterator) isBreak(last, typ byte) bool {
	if typ&last != 0 {
		return false
	}
	switch {
	case !it.o.splitOnCaseChange && last&wdAlpha != 0 && typ&wdAlpha != 0:
		return false
	case last&wdUpper != 0 && typ&wdAlpha != 0:
		return false
	case !it.o.splitOnNumerics && ((last&wdAlpha != 0 && typ&wdDigit != 0) || (last&wdDigit != 0 && typ&wdAlpha != 0)):
		return false
	}
	return true
}

func (it *wdIterator) next() int {
	it.current = it.end
	if it.current == wdDone {
		return wdDone
	}
	if it.skipPossessive {
		it.current += 2
		it.skipPossessive = false
	}
	var last byte
	for it.current < it.endBounds {
		last = it.o.charType(it.text[it.current])
		if last&wdSubwordDelim == 0 {
			break
		}
		it.current++
	}
	if it.current >= it.endBounds {
		it.end = wdDone
		return wdDone
	}
	for it.end = it.current + 1; it.end < it.endBounds; it.end++ {
		typ := it.o.charType(it.text[it.end])
		if it.isBreak(last, typ) {
			break
		}
		last = typ
	}
	if it.end < it.endBounds-1 && it.endsWithPossessive(it.end+2) {
		it.skipPossessive = true
	}
	return it.end
}

func (it *wdIterator) typ() byte {
	if it.end == wdDone {
		return 0
	}
	t := it.o.charType(it.text[it.current])
	if t == wdLower || t == wdUpper {
		return wdAlpha
	}
	return t
}

func (it *wdIterator) isSingleWord() bool {
	if it.hasFinalPossessive {
		return it.current == it.startBounds && it.end == it.endBounds-2
	}
	return it.current == it.startBounds && it.end == it.endBounds
}

func (o *wordDelimiterOptions) shouldConcatenate(t byte) bool {
	return (o.catenateWords && t&wdAlpha != 0) || (o.catenateNumbers && t&wdDigit != 0)
}

func (o *wordDelimiterOptions) shouldGenerateParts(t byte) bool {
	return (o.generateWordParts && t&wdAlpha != 0) || (o.generateNumberParts && t&wdDigit != 0)
}

type wdConcat struct {
	text               []uint16
	typ                byte
	startPart, endPart int
	startPos           int
	count              int
}

func (c *wdConcat) empty() bool { return len(c.text) == 0 }

func (c *wdConcat) reset() { *c = wdConcat{} }

// wordDelimiterGraphFilter is WordDelimiterGraphFilter.
func wordDelimiterGraphFilter(o *wordDelimiterOptions) tokenFunc {
	type part struct {
		term               []uint16 // nil: slice of the saved term
		startPos, endPos   int
		startPart, endPart int
	}
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		accumPosInc := 0
		for _, t := range in.tokens {
			if o.ignoreKeywords && t.keyword {
				out.tokens = append(out.tokens, t)
				continue
			}
			text := utf16.Encode([]rune(t.term))
			accumPosInc += t.posInc
			it := newWDIterator(o, text)
			it.next()
			if (it.current == 0 && it.end == len(text)) || o.protected.contains(t.term) {
				t.posInc = accumPosInc
				accumPosInc = 0
				out.tokens = append(out.tokens, t)
				continue
			}
			if it.end == wdDone {
				if o.preserveOriginal {
					accumPosInc = 0
					out.tokens = append(out.tokens, t)
				}
				continue
			}
			var parts []part
			wordPos, lastConcatCount := 0, 0
			var concat, concatAll wdConcat
			if o.preserveOriginal {
				parts = append(parts, part{nil, 0, 1, 0, len(text)})
			}
			write := func(c *wdConcat) {
				parts = append(parts, part{append([]uint16(nil), c.text...), c.startPos, wordPos, c.startPart, c.endPart})
			}
			flush := func(c *wdConcat) {
				if wordPos == c.startPos {
					wordPos++
				}
				lastConcatCount = c.count
				if c.count != 1 || !o.shouldGenerateParts(c.typ) {
					write(c)
				}
				c.reset()
			}
			add := func(c *wdConcat) {
				if c.empty() {
					c.typ = it.typ()
					c.startPart = it.current
					c.startPos = wordPos
				}
				c.text = append(c.text, text[it.current:it.end]...)
				c.endPart = it.end
				c.count++
			}
			if it.isSingleWord() {
				parts = append(parts, part{nil, wordPos, wordPos + 1, it.current, it.end})
				wordPos++
				it.next()
			} else {
				for it.end != wdDone {
					wt := it.typ()
					if !concat.empty() && concat.typ&wt == 0 {
						flush(&concat)
					}
					if o.shouldConcatenate(wt) {
						add(&concat)
					}
					if o.catenateAll {
						add(&concatAll)
					}
					if o.shouldGenerateParts(wt) {
						parts = append(parts, part{nil, wordPos, wordPos + 1, it.current, it.end})
						wordPos++
					}
					it.next()
				}
				if !concat.empty() {
					flush(&concat)
				}
				if !concatAll.empty() {
					if concatAll.count > lastConcatCount {
						if wordPos == concatAll.startPos {
							wordPos++
						}
						write(&concatAll)
					}
					concatAll.reset()
				}
			}
			if o.preserveOriginal {
				if wordPos == 0 {
					wordPos++
				}
				parts[0].endPos = wordPos
			}
			sort.SliceStable(parts, func(i, j int) bool {
				if parts[i].startPos != parts[j].startPos {
					return parts[i].startPos < parts[j].startPos
				}
				return parts[i].endPos > parts[j].endPos
			})
			adjusting := o.adjustOffsets && t.end-t.start == len(text)
			lastStart := 0
			wordPos = 0
			for _, p := range parts {
				k := t
				start, end := t.start, t.end
				if adjusting {
					start, end = t.start+p.startPart, t.start+p.endPart
				}
				if start < lastStart {
					start = lastStart
				}
				if end < lastStart {
					end = lastStart
				}
				lastStart = start
				k.start, k.end = start, end
				if p.term == nil {
					k.term = string(utf16.Decode(text[p.startPart:p.endPart]))
				} else {
					k.term = string(utf16.Decode(p.term))
				}
				k.posInc = accumPosInc + p.startPos - wordPos
				accumPosInc = 0
				k.posLen = p.endPos - p.startPos
				wordPos = p.startPos
				out.tokens = append(out.tokens, k)
			}
		}
		return out
	}
}

// wordDelimiterFilter is the deprecated WordDelimiterFilter.
func wordDelimiterFilter(o *wordDelimiterOptions) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		accumPosInc := 0
		first := true
		for _, t := range in.tokens {
			if o.ignoreKeywords && t.keyword {
				out.tokens = append(out.tokens, t)
				continue
			}
			text := utf16.Encode([]rune(t.term))
			accumPosInc += t.posInc
			it := newWDIterator(o, text)
			it.next()
			if (it.current == 0 && it.end == len(text)) || o.protected.contains(t.term) {
				t.posInc = accumPosInc
				accumPosInc = 0
				first = false
				out.tokens = append(out.tokens, t)
				continue
			}
			if it.end == wdDone && !o.preserveOriginal {
				if t.posInc == 1 && !first {
					accumPosInc--
				}
				continue
			}
			hasOutputToken := false
			hasOutputFollowingOriginal := !o.preserveOriginal
			lastConcatCount := 0
			illegalOffsets := t.end-t.start != len(text)
			if o.preserveOriginal {
				k := t
				k.posInc = accumPosInc
				accumPosInc = 0
				first = false
				out.tokens = append(out.tokens, k)
			}
			position := func(inject bool) int {
				posInc := accumPosInc
				if hasOutputToken {
					accumPosInc = 0
					if inject {
						return 0
					}
					if posInc < 1 {
						return 1
					}
					return posInc
				}
				hasOutputToken = true
				if !hasOutputFollowingOriginal {
					hasOutputFollowingOriginal = true
					return 0
				}
				accumPosInc = 0
				if posInc < 1 {
					return 1
				}
				return posInc
			}
			var buffered []anToken
			var concat, concatAll wdConcat
			writeConcat := func(c *wdConcat) {
				k := t
				k.term = string(utf16.Decode(c.text))
				if illegalOffsets {
					k.start, k.end = t.start, t.end
				} else {
					k.start, k.end = t.start+c.startPart, t.start+c.endPart
				}
				k.posInc = position(true)
				k.posLen = 1
				accumPosInc = 0
				buffered = append(buffered, k)
				c.reset()
			}
			flush := func(c *wdConcat) bool {
				lastConcatCount = c.count
				if c.count != 1 || !o.shouldGenerateParts(c.typ) {
					writeConcat(c)
					return true
				}
				c.reset()
				return false
			}
			add := func(c *wdConcat) {
				if c.empty() {
					c.startPart = it.current
				}
				c.text = append(c.text, text[it.current:it.end]...)
				c.endPart = it.end
				c.count++
			}
			generatePart := func(single bool) anToken {
				k := t
				k.term = string(utf16.Decode(text[it.current:it.end]))
				start, end := t.start+it.current, t.start+it.end
				if illegalOffsets {
					if single && start <= t.end {
						k.start, k.end = start, t.end
					} else {
						k.start, k.end = t.start, t.end
					}
				} else {
					k.start, k.end = start, end
				}
				k.posInc = position(false)
				k.posLen = 1
				return k
			}
			for it.end != wdDone {
				if it.isSingleWord() {
					out.tokens = append(out.tokens, generatePart(true))
					it.next()
					first = false
					continue
				}
				wt := it.typ()
				if !concat.empty() && concat.typ&wt == 0 {
					flush(&concat)
					hasOutputToken = false
				}
				if o.shouldConcatenate(wt) {
					if concat.empty() {
						concat.typ = wt
					}
					add(&concat)
				}
				if o.catenateAll {
					add(&concatAll)
				}
				if o.shouldGenerateParts(wt) {
					buffered = append(buffered, generatePart(false))
				}
				it.next()
			}
			if !concat.empty() {
				flush(&concat)
			}
			if !concatAll.empty() {
				if concatAll.count > lastConcatCount {
					writeConcat(&concatAll)
				}
				concatAll.reset()
			}
			sort.SliceStable(buffered, func(i, j int) bool {
				if buffered[i].start != buffered[j].start {
					return buffered[i].start < buffered[j].start
				}
				return buffered[i].posInc > buffered[j].posInc
			})
			for _, k := range buffered {
				if first && k.posInc == 0 {
					k.posInc = 1
				}
				first = false
				out.tokens = append(out.tokens, k)
			}
		}
		return out
	}
}

// synonymMap is Lucene's SynonymMap (inputs and outputs are word lists).
type synonymMap struct {
	entries map[string]*synonymEntry
	maxLen  int
}

type synonymEntry struct {
	keepOrig bool
	outputs  [][]string
	seen     map[string]bool
}

func (m *synonymMap) add(in, out []string, includeOrig bool) {
	key := strings.Join(in, "\x00")
	e := m.entries[key]
	if e == nil {
		e = &synonymEntry{seen: map[string]bool{}}
		m.entries[key] = e
	}
	e.keepOrig = e.keepOrig || includeOrig
	if ok := strings.Join(out, "\x00"); !e.seen[ok] {
		e.seen[ok] = true
		e.outputs = append(e.outputs, out)
	}
	if len(in) > m.maxLen {
		m.maxLen = len(in)
	}
}

// longest finds the longest rule matching the tokens at i.
func (m *synonymMap) longest(tokens []anToken, i int) (int, *synonymEntry) {
	var words []string
	bestLen, best := 0, (*synonymEntry)(nil)
	for l := 1; l <= m.maxLen && i+l <= len(tokens); l++ {
		words = append(words, tokens[i+l-1].term)
		if e, ok := m.entries[strings.Join(words, "\x00")]; ok {
			bestLen, best = l, e
		}
	}
	return bestLen, best
}

func solrSplit(s, sep string) []string {
	var out []string
	var sb strings.Builder
	for pos := 0; pos < len(s); {
		if strings.HasPrefix(s[pos:], sep) {
			if sb.Len() > 0 {
				out = append(out, sb.String())
				sb.Reset()
			}
			pos += len(sep)
			continue
		}
		ch := s[pos]
		pos++
		if ch == '\\' {
			sb.WriteByte(ch)
			if pos >= len(s) {
				break
			}
			ch = s[pos]
			pos++
		}
		sb.WriteByte(ch)
	}
	if sb.Len() > 0 {
		out = append(out, sb.String())
	}
	return out
}

func solrUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i < len(s)-1 {
			i++
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

var errSynonyms = &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Failed to build synonyms"}

// parseSolrSynonyms builds a synonym map from Solr-format rules analyzed with
// the chain preceding the synonym filter.
func parseSolrSynonyms(rules []string, expand, lenient bool, chain *anAnalyzer) (*synonymMap, error) {
	m := &synonymMap{entries: map[string]*synonymEntry{}}
	analyze := func(text string) ([]string, bool) {
		s := chain.analyze(text)
		var words []string
		for _, t := range s.tokens {
			if t.term == "" || t.posInc != 1 {
				return nil, false
			}
			words = append(words, t.term)
		}
		return words, len(words) > 0
	}
	sides := func(parts []string) ([][]string, error) {
		var out [][]string
		for _, p := range parts {
			w, ok := analyze(strings.TrimSpace(solrUnescape(p)))
			if !ok {
				if lenient {
					out = append(out, nil)
					continue
				}
				return nil, errSynonyms
			}
			out = append(out, w)
		}
		return out, nil
	}
	add := func(in, out []string, keepOrig bool) {
		if in != nil && out != nil {
			m.add(in, out, keepOrig)
		}
	}
	for _, rule := range rules {
		for _, line := range strings.Split(rule, "\n") {
			if line == "" || line[0] == '#' {
				continue
			}
			parts := solrSplit(line, "=>")
			if len(parts) > 2 {
				return nil, errSynonyms
			}
			if len(parts) == 2 {
				ins, err := sides(solrSplit(parts[0], ","))
				if err != nil {
					return nil, err
				}
				outs, err := sides(solrSplit(parts[1], ","))
				if err != nil {
					return nil, err
				}
				for _, in := range ins {
					for _, out := range outs {
						add(in, out, false)
					}
				}
				continue
			}
			ins, err := sides(solrSplit(line, ","))
			if err != nil {
				return nil, err
			}
			if expand {
				for i := range ins {
					for j := range ins {
						if i != j {
							add(ins[i], ins[j], true)
						}
					}
				}
			} else {
				for i := range ins {
					if len(ins) > 0 {
						add(ins[i], ins[0], false)
					}
				}
			}
		}
	}
	return m, nil
}

// synonymGraphFilter is SynonymGraphFilter.
func synonymGraphFilter(m *synonymMap) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		lastNodeOut, nextNodeOut := -1, 0
		for i := 0; i < len(in.tokens); {
			l, e := m.longest(in.tokens, i)
			if e == nil {
				t := in.tokens[i]
				lastNodeOut += t.posInc
				posLen := t.posLen
				if posLen < 1 {
					posLen = 1
				}
				nextNodeOut = lastNodeOut + posLen
				out.tokens = append(out.tokens, t)
				i++
				continue
			}
			type buffered struct {
				tok        anToken
				start, end int
			}
			matchStart, matchEnd := in.tokens[i].start, in.tokens[i+l-1].end
			syn := func(term string) anToken {
				return anToken{term: term, start: matchStart, end: matchEnd, typ: "SYNONYM"}
			}
			total := 0
			if e.keepOrig {
				total = l - 1
			}
			for _, p := range e.outputs {
				total += len(p) - 1
			}
			startNode := nextNodeOut
			endNode := startNode + total + 1
			var buf []buffered
			newNodes := 0
			for _, p := range e.outputs {
				pathEnd := endNode
				if len(p) > 1 {
					pathEnd = nextNodeOut + newNodes + 1
					newNodes += len(p) - 1
				}
				buf = append(buf, buffered{syn(p[0]), startNode, pathEnd})
			}
			if e.keepOrig {
				inputEnd := endNode
				if l > 1 {
					inputEnd = nextNodeOut + newNodes + 1
				}
				buf = append(buf, buffered{in.tokens[i], startNode, inputEnd})
			}
			nextNodeOut = endNode
			for pi, p := range e.outputs {
				if len(p) > 1 {
					last := buf[pi].end
					for k := 1; k < len(p)-1; k++ {
						buf = append(buf, buffered{syn(p[k]), last, last + 1})
						last++
					}
					buf = append(buf, buffered{syn(p[len(p)-1]), last, endNode})
				}
			}
			if e.keepOrig && l > 1 {
				last := buf[len(e.outputs)].end
				for k := 1; k < l-1; k++ {
					buf = append(buf, buffered{in.tokens[i+k], last, last + 1})
					last++
				}
				buf = append(buf, buffered{in.tokens[i+l-1], last, endNode})
			}
			for _, b := range buf {
				t := b.tok
				t.posInc = b.start - lastNodeOut
				lastNodeOut = b.start
				t.posLen = b.end - b.start
				out.tokens = append(out.tokens, t)
			}
			i += l
		}
		return out
	}
}

// synonymFilter is the deprecated SynonymFilter.
func synonymFilter(m *synonymMap) tokenFunc {
	return func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		type pendingOutput struct {
			term   string
			end    int // -1: the end offset of the input at this slot
			posLen int
		}
		n := len(in.tokens)
		outputs := map[int][]pendingOutput{}
		matched := make([]bool, n)
		keepOrig := make([]bool, n)
		maxSlot := n
		for i := 0; i < n; {
			l, e := m.longest(in.tokens, i)
			if e == nil {
				i++
				continue
			}
			matchEnd := in.tokens[i+l-1].end
			for _, p := range e.outputs {
				for k, w := range p {
					po := pendingOutput{term: w, end: -1, posLen: 1}
					if len(p) == 1 {
						po.end = matchEnd
						if e.keepOrig {
							po.posLen = l
						}
					}
					outputs[i+k] = append(outputs[i+k], po)
					if i+k+1 > maxSlot {
						maxSlot = i + k + 1
					}
				}
			}
			for k := 0; k < l; k++ {
				matched[i+k] = true
				keepOrig[i+k] = keepOrig[i+k] || e.keepOrig
			}
			i += l
		}
		lastStart, lastEnd := 0, 0
		for slot := 0; slot < maxSlot; slot++ {
			posInc := 1
			if slot < n {
				t := in.tokens[slot]
				lastStart, lastEnd = t.start, t.end
				if !matched[slot] || keepOrig[slot] {
					out.tokens = append(out.tokens, t)
					posInc = 0
				}
				for _, po := range outputs[slot] {
					end := po.end
					if end < 0 {
						end = t.end
					}
					out.tokens = append(out.tokens, anToken{term: po.term, start: t.start, end: end, posInc: posInc, posLen: po.posLen, typ: "SYNONYM"})
					posInc = 0
				}
				continue
			}
			for _, po := range outputs[slot] {
				out.tokens = append(out.tokens, anToken{term: po.term, start: lastStart, end: lastEnd, posInc: posInc, posLen: 1, typ: "SYNONYM"})
				posInc = 0
			}
		}
		return out
	}
}

// ---------------------------------------------------------------------------
// char filters
// ---------------------------------------------------------------------------

// mappingCharFilter is Lucene's MappingCharFilter (greedy longest match).
type mappingCharFilter struct {
	rules  map[string]string
	maxLen int // longest key in UTF-16 units
}

func (f *mappingCharFilter) filter(text string) (string, *offsetMap) {
	in := utf16.Encode([]rune(text))
	var out []uint16
	m := &offsetMap{}
	for i := 0; i < len(in); {
		matchLen := -1
		var repl string
		for l := 1; l <= f.maxLen && i+l <= len(in); l++ {
			if r, ok := f.rules[string(utf16.Decode(in[i:i+l]))]; ok {
				matchLen, repl = l, r
			}
		}
		if matchLen < 0 {
			out = append(out, in[i])
			i++
			continue
		}
		i += matchLen
		replU := utf16.Encode([]rune(repl))
		diff := matchLen - len(replU)
		if diff != 0 {
			prev := m.lastDiff()
			if diff > 0 {
				m.add(i-diff-prev, prev+diff)
			} else {
				outputStart := i - prev
				for extra := 0; extra < -diff; extra++ {
					m.add(outputStart+extra, prev-extra-1)
				}
			}
		}
		out = append(out, replU...)
	}
	return string(utf16.Decode(out)), m
}

// patternReplaceCharFilter is Lucene's PatternReplaceCharFilter.
type patternReplaceCharFilter struct {
	re          *regexp.Regexp
	replacement string
}

func (f *patternReplaceCharFilter) filter(text string) (string, *offsetMap) {
	u := utf16Positions(text)
	m := &offsetMap{}
	var sb strings.Builder
	cumulative, last := 0, 0
	outLen := 0 // UTF-16 length of sb
	for _, match := range f.re.FindAllStringSubmatchIndex(text, -1) {
		skipped := text[last:match[0]]
		sb.WriteString(skipped)
		outLen += utf16Len(skipped)
		repl := javaReplacement(f.replacement, text, match)
		sb.WriteString(repl)
		groupSize := u(match[1]) - u(match[0])
		replSize := utf16Len(repl)
		lengthBefore := outLen
		outLen += replSize
		last = match[1]
		if groupSize != replSize {
			if replSize < groupSize {
				cumulative += groupSize - replSize
				m.add(lengthBefore+replSize, cumulative)
			} else {
				for i := groupSize; i < replSize; i++ {
					cumulative--
					m.add(lengthBefore+i, cumulative)
				}
			}
		}
	}
	sb.WriteString(text[last:])
	return sb.String(), m
}

// javaReplacement expands a java.util.regex replacement string ($n group
// references, backslash escapes).
func javaReplacement(repl, text string, match []int) string {
	var sb strings.Builder
	groups := len(match)/2 - 1
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		switch {
		case c == '\\' && i+1 < len(repl):
			i++
			sb.WriteByte(repl[i])
		case c == '$' && i+1 < len(repl) && repl[i+1] >= '0' && repl[i+1] <= '9':
			i++
			g := int(repl[i] - '0')
			for i+1 < len(repl) && repl[i+1] >= '0' && repl[i+1] <= '9' {
				ng := g*10 + int(repl[i+1]-'0')
				if ng > groups {
					break
				}
				g = ng
				i++
			}
			if g <= groups && match[2*g] >= 0 {
				sb.WriteString(text[match[2*g]:match[2*g+1]])
			}
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// checkJavaReplacement reports the error java.util.regex raises for a group
// reference beyond the pattern's groups.
func checkJavaReplacement(repl string, groups int) error {
	for i := 0; i < len(repl); i++ {
		switch repl[i] {
		case '\\':
			i++
		case '$':
			if i+1 >= len(repl) {
				return &Error{Status: 500, Type: "illegal_argument_exception", Reason: "Illegal group reference: group index is missing"}
			}
			if repl[i+1] < '0' || repl[i+1] > '9' {
				if repl[i+1] == '{' {
					continue
				}
				return &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Illegal group reference"}
			}
			if g := int(repl[i+1] - '0'); g > groups {
				return &Error{Status: 500, Type: "index_out_of_bounds_exception", Reason: fmt.Sprintf("No group %d", g)}
			}
		}
	}
	return nil
}

// htmlStripCharFilter approximates Lucene's HTMLStripCharFilter: tags are
// removed (block-level elements and <br> become a newline), comments,
// processing instructions, declarations, <script> and <style> contents are
// removed, CDATA sections are unwrapped and character entities decoded.
type htmlStripCharFilter struct {
	escaped map[string]bool
}

var htmlBlockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "br": true, "caption": true, "center": true, "dd": true, "dir": true,
	"div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true, "footer": true, "form": true, "frame": true, "frameset": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "header": true, "hgroup": true, "hr": true, "iframe": true, "isindex": true,
	"li": true, "map": true, "menu": true, "nav": true, "noframes": true, "noscript": true, "object": true, "ol": true, "output": true, "p": true, "pre": true,
	"section": true, "select": true, "table": true, "tbody": true, "td": true, "textarea": true, "tfoot": true, "th": true, "thead": true, "tr": true, "ul": true,
	"body": true, "head": true, "html": true, "title": true, "option": true, "optgroup": true, "applet": true, "legend": true, "img": false,
}

func (f *htmlStripCharFilter) filter(text string) (string, *offsetMap) {
	in := utf16.Encode([]rune(text))
	var out []uint16
	m := &offsetMap{}
	cumulative := 0
	// replace in[start:end] with repl and record the correction at the end of
	// the replacement
	replace := func(start, end int, repl []uint16) {
		out = append(out, repl...)
		if d := (end - start) - len(repl); d != 0 {
			cumulative += d
			m.add(len(out), cumulative)
		}
	}
	lower := func(s []uint16) string { return strings.ToLower(string(utf16.Decode(s))) }
	indexOf := func(from int, pat string) int {
		p := utf16.Encode([]rune(pat))
		for i := from; i+len(p) <= len(in); i++ {
			ok := true
			for k := range p {
				c := in[i+k]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				if c != p[k] {
					ok = false
					break
				}
			}
			if ok {
				return i
			}
		}
		return -1
	}
	for i := 0; i < len(in); {
		c := in[i]
		if c == '&' {
			if repl, n := decodeHTMLEntity(in[i:]); n > 0 {
				replace(i, i+n, repl)
				i += n
				continue
			}
			out = append(out, c)
			i++
			continue
		}
		if c != '<' || i+1 >= len(in) {
			out = append(out, c)
			i++
			continue
		}
		rest := in[i+1:]
		switch {
		case len(rest) >= 3 && rest[0] == '!' && rest[1] == '-' && rest[2] == '-':
			end := indexOf(i+4, "-->")
			if end < 0 {
				out = append(out, c)
				i++
				continue
			}
			replace(i, end+3, nil)
			i = end + 3
			continue
		case len(rest) >= 8 && lower(rest[:8]) == "![cdata[":
			end := indexOf(i+9, "]]>")
			if end < 0 {
				out = append(out, c)
				i++
				continue
			}
			replace(i, i+9, nil)
			out = append(out, in[i+9:end]...)
			replace(end, end+3, nil)
			i = end + 3
			continue
		case rest[0] == '!' || rest[0] == '?':
			end := indexOf(i+1, ">")
			if end < 0 {
				out = append(out, c)
				i++
				continue
			}
			replace(i, end+1, nil)
			i = end + 1
			continue
		}
		closing := rest[0] == '/'
		j := i + 1
		if closing {
			j++
		}
		k := j
		for k < len(in) && (in[k] < 0x80 && (unicode.IsLetter(rune(in[k])) || unicode.IsDigit(rune(in[k])) || in[k] == '-' || in[k] == '_' || in[k] == ':')) {
			k++
		}
		if k == j || !(unicode.IsLetter(rune(in[j]))) {
			out = append(out, c)
			i++
			continue
		}
		name := lower(in[j:k])
		// find the end of the tag, honouring quoted attribute values
		end, quote := -1, uint16(0)
		for p := k; p < len(in); p++ {
			ch := in[p]
			if quote != 0 {
				if ch == quote {
					quote = 0
				}
				continue
			}
			if ch == '"' || ch == '\'' {
				quote = ch
				continue
			}
			if ch == '>' {
				end = p
				break
			}
			if ch == '<' {
				break
			}
		}
		if end < 0 {
			if quote == 0 && indexOf(k, "<") < 0 {
				// an unterminated tag at the end of the input is dropped
				replace(i, len(in), nil)
				i = len(in)
				continue
			}
			out = append(out, c)
			i++
			continue
		}
		if f.escaped[name] {
			out = append(out, in[i:end+1]...)
			i = end + 1
			continue
		}
		if !closing && (name == "script" || name == "style") {
			close := indexOf(end+1, "</"+name)
			if close >= 0 {
				if gt := indexOf(close, ">"); gt >= 0 {
					replace(i, gt+1, []uint16{'\n'})
					i = gt + 1
					continue
				}
			}
		}
		if htmlBlockTags[name] {
			replace(i, end+1, []uint16{'\n'})
		} else {
			replace(i, end+1, nil)
		}
		i = end + 1
	}
	return string(utf16.Decode(out)), m
}

var htmlEntities = map[string]rune{
	"amp": '&', "lt": '<', "gt": '>', "quot": '"', "apos": '\'', "nbsp": ' ', "copy": 0xA9, "reg": 0xAE, "trade": 0x2122,
	"eacute": 0xE9, "egrave": 0xE8, "ecirc": 0xEA, "euml": 0xEB, "aacute": 0xE1, "agrave": 0xE0, "acirc": 0xE2, "auml": 0xE4, "aring": 0xE5,
	"atilde": 0xE3, "aelig": 0xE6, "ccedil": 0xE7, "iacute": 0xED, "igrave": 0xEC, "icirc": 0xEE, "iuml": 0xEF, "ntilde": 0xF1,
	"oacute": 0xF3, "ograve": 0xF2, "ocirc": 0xF4, "ouml": 0xF6, "otilde": 0xF5, "oslash": 0xF8, "uacute": 0xFA, "ugrave": 0xF9,
	"ucirc": 0xFB, "uuml": 0xFC, "yacute": 0xFD, "yuml": 0xFF, "szlig": 0xDF, "Eacute": 0xC9, "Egrave": 0xC8, "Aacute": 0xC1,
	"Agrave": 0xC0, "Auml": 0xC4, "Ouml": 0xD6, "Uuml": 0xDC, "Ccedil": 0xC7, "Ntilde": 0xD1, "euro": 0x20AC, "pound": 0xA3,
	"yen": 0xA5, "cent": 0xA2, "sect": 0xA7, "deg": 0xB0, "middot": 0xB7, "laquo": 0xAB, "raquo": 0xBB, "hellip": 0x2026,
	"mdash": 0x2014, "ndash": 0x2013, "lsquo": 0x2018, "rsquo": 0x2019, "ldquo": 0x201C, "rdquo": 0x201D, "bull": 0x2022,
	"times": 0xD7, "divide": 0xF7, "plusmn": 0xB1, "frac12": 0xBD, "frac14": 0xBC, "frac34": 0xBE, "iexcl": 0xA1, "iquest": 0xBF,
	"shy": 0xAD, "micro": 0xB5, "para": 0xB6, "ordf": 0xAA, "ordm": 0xBA, "sup1": 0xB9, "sup2": 0xB2, "sup3": 0xB3,
}

// decodeHTMLEntity decodes a character reference at the start of in and
// returns its replacement and length (0 when there is none).
func decodeHTMLEntity(in []uint16) ([]uint16, int) {
	end := -1
	for k := 1; k < len(in) && k < 12; k++ {
		if in[k] == ';' {
			end = k
			break
		}
	}
	if end < 2 {
		return nil, 0
	}
	name := string(utf16.Decode(in[1:end]))
	if name[0] == '#' {
		var v int64
		var err error
		if len(name) > 1 && (name[1] == 'x' || name[1] == 'X') {
			v, err = strconv.ParseInt(name[2:], 16, 32)
		} else {
			v, err = strconv.ParseInt(name[1:], 10, 32)
		}
		if err != nil || v < 0 || v > unicode.MaxRune {
			return nil, 0
		}
		return utf16.Encode([]rune{rune(v)}), end + 1
	}
	if r, ok := htmlEntities[name]; ok {
		return utf16.Encode([]rune{r}), end + 1
	}
	return nil, 0
}

// ---------------------------------------------------------------------------
// component registry
// ---------------------------------------------------------------------------

// anContext is the environment analysis components are built in: the
// analysis settings of an index, or none for index-less _analyze requests.
type anContext struct {
	hasIndex       bool
	analysis       M
	maxNgramDiff   int
	maxShingleDiff int
	maxTokenCount  int
	depth          int
}

func newAnContext(settings M, hasIndex bool) *anContext {
	ctx := &anContext{hasIndex: hasIndex, analysis: analysisSettings(settings), maxNgramDiff: 1, maxShingleDiff: 3, maxTokenCount: 10000}
	if v, ok := indexSettingValue(settings, "max_ngram_diff"); ok {
		ctx.maxNgramDiff = int(toInt64(v, 1))
	}
	if v, ok := indexSettingValue(settings, "max_shingle_diff"); ok {
		ctx.maxShingleDiff = int(toInt64(v, 3))
	}
	if v, ok := indexSettingValue(settings, "analyze.max_token_count"); ok {
		ctx.maxTokenCount = int(toInt64(v, 10000))
	}
	return ctx
}

// indexSettingValue reads index.<key> from nested or flat settings.
func indexSettingValue(settings M, key string) (any, bool) {
	if settings == nil {
		return nil, false
	}
	if v, ok := settings["index."+key]; ok {
		return v, true
	}
	idx := getMap(settings, "index")
	if idx == nil {
		return nil, false
	}
	if v, ok := idx[key]; ok {
		return v, true
	}
	cur := any(idx)
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(M)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func toInt64(v any, def int64) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n
		}
	}
	return def
}

// unsupportedComponent reports a valid OpenSearch component osmem cannot
// emulate. Index creation turns it into a warning; _analyze rejects it.
type unsupportedComponent struct{ what string }

func (u *unsupportedComponent) Error() string { return u.what + " is not supported by osmem" }

func unsupported(format string, args ...any) error {
	return &unsupportedComponent{what: fmt.Sprintf(format, args...)}
}

// compBuild carries what a component factory needs.
type compBuild struct {
	ctx   *anContext
	name  string
	kind  string // "tokenizer", "char_filter", "filter"
	s     M
	chain *anAnalyzer // for token filters: the chain before the filter
}

func (b *compBuild) raw(key string) (any, bool) {
	v, ok := b.s[key]
	return v, ok && v != nil
}

func (b *compBuild) str(key, def string) string {
	v, ok := b.raw(key)
	if !ok {
		return def
	}
	return analysisSettingString(v)
}

func analysisSettingString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = analysisSettingString(e)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

func (b *compBuild) int(key string, def int) (int, error) {
	v, ok := b.raw(key)
	if !ok {
		return def, nil
	}
	s := analysisSettingString(v)
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, &Error{Status: 400, Type: "illegal_argument_exception", Reason: fmt.Sprintf("Failed to parse value [%s] for setting [%s]", s, key)}
	}
	return n, nil
}

func (b *compBuild) bool(key string, def bool) (bool, error) {
	v, ok := b.raw(key)
	if !ok {
		return def, nil
	}
	switch s := analysisSettingString(v); s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Failed to parse value [" + s + "] as only [true] or [false] are allowed."}
	}
}

// list reads a list setting (an array or a comma-separated string).
func (b *compBuild) list(key string) ([]string, bool) {
	v, ok := b.raw(key)
	if !ok {
		return nil, false
	}
	if arr, isArr := v.([]any); isArr {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			out = append(out, analysisSettingString(e))
		}
		return out, true
	}
	var out []string
	for _, p := range strings.Split(analysisSettingString(v), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, len(out) > 0
}

// flatSettingsJSON renders a component definition as OpenSearch Settings
// print it (flat keys, string values).
func flatSettingsJSON(def M) string {
	flat := map[string]any{}
	var walk func(prefix string, m M)
	walk = func(prefix string, m M) {
		for k, v := range m {
			switch x := v.(type) {
			case M:
				walk(prefix+k+".", x)
			case []any:
				arr := make([]any, len(x))
				for i, e := range x {
					arr[i] = analysisSettingString(e)
				}
				flat[prefix+k] = arr
			case nil:
				flat[prefix+k] = nil
			default:
				flat[prefix+k] = analysisSettingString(x)
			}
		}
	}
	walk("", def)
	data, _ := json.Marshal(flat)
	return string(data)
}

func componentKindName(kind string) string {
	if kind == "filter" {
		return "token filter"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

func errAnalysis(format string, args ...any) *Error {
	return &Error{Status: 400, Type: "illegal_argument_exception", Reason: fmt.Sprintf(format, args...)}
}

// withoutType returns a definition without its type key.
func withoutType(def M) M {
	out := make(M, len(def))
	for k, v := range def {
		if k != "type" {
			out[k] = v
		}
	}
	return out
}

// requiresSettings lists global components that cannot be built without
// settings.
var requiresSettings = map[string]map[string]bool{
	"filter": {"condition": true, "dictionary_decompounder": true, "hyphenation_decompounder": true, "keep": true, "keep_types": true,
		"keyword_marker": true, "pattern_capture": true, "pattern_replace": true, "predicate_token_filter": true, "stemmer_override": true,
		"synonym": true, "synonym_graph": true, "hunspell": true},
	"char_filter": {"mapping": true, "pattern_replace": true},
}

// deprecatedComponentError reports names OpenSearch 3 rejects.
func deprecatedComponentError(kind, name string) error {
	switch {
	case kind == "tokenizer" && (name == "nGram" || name == "edgeNGram"):
		return errAnalysis("The [%s] tokenizer name was deprecated pre 1.0. Please change the tokenizer name to [%s] for indices created in versions 3.0 or higher instead.", name, map[string]string{"nGram": "ngram", "edgeNGram": "edge_ngram"}[name])
	case kind == "filter" && (name == "nGram" || name == "edgeNGram"):
		return errAnalysis("The [%s] token filter name was deprecated in 6.4 and cannot be used in new indices. Please change the filter name to [%s] instead.", name, map[string]string{"nGram": "ngram", "edgeNGram": "edge_ngram"}[name])
	case kind == "filter" && name == "standard":
		return errAnalysis("The [standard] token filter has been removed.")
	}
	return nil
}

// definition resolves a component reference (a name or an inline
// definition) to its name, type and settings, the way
// AnalysisRegistry.getComponentFactory does.
func (ctx *anContext) definition(kind string, nod any) (name, typ string, s M, err error) {
	switch v := nod.(type) {
	case M:
		typ, _ = v["type"].(string)
		if _, has := v["type"]; has && typ == "" {
			typ = analysisSettingString(v["type"])
		}
		if typ == "" {
			return "", "", nil, errAnalysis("Missing [type] setting for anonymous %s: %s", kind, flatSettingsJSON(v))
		}
		if !knownComponentType(kind, typ) {
			return "", "", nil, errAnalysis("failed to find global %s under [%s]", kind, typ)
		}
		return "__anonymous__" + typ, typ, withoutType(v), nil
	case string:
		if err := deprecatedComponentError(kind, v); err != nil {
			return "", "", nil, err
		}
		if ctx.hasIndex {
			if def, ok := getMap(ctx.analysis, kind)[v].(M); ok {
				typ = getString(def, "type")
				return v, typ, withoutType(def), nil
			}
			if !knownComponentType(kind, v) {
				return "", "", nil, errAnalysis("failed to find %s under [%s]", kind, v)
			}
			return v, v, M{}, nil
		}
		if !knownComponentType(kind, v) {
			return "", "", nil, errAnalysis("failed to find global %s under [%s]", kind, v)
		}
		if requiresSettings[kind][v] {
			return "", "", nil, errAnalysis("Analysis settings required - can't instantiate analysis factory")
		}
		return v, v, M{}, nil
	}
	return "", "", nil, errAnalysis("failed to find global %s under [%v]", kind, nod)
}

func (ctx *anContext) charFilter(nod any) (namedCharFilter, error) {
	name, typ, s, err := ctx.definition("char_filter", nod)
	if err != nil {
		return namedCharFilter{}, err
	}
	f := charFilterTypes[typ]
	if f == nil {
		return namedCharFilter{}, errAnalysis("Unknown char_filter type [%s] for [%s]", typ, name)
	}
	cf, err := f(&compBuild{ctx: ctx, name: name, kind: "char_filter", s: s})
	return namedCharFilter{name: name, cf: cf}, err
}

func (ctx *anContext) tokenizer(nod any) (namedTokenizer, error) {
	name, typ, s, err := ctx.definition("tokenizer", nod)
	if err != nil {
		return namedTokenizer{}, err
	}
	f := tokenizerTypes[typ]
	if f == nil {
		return namedTokenizer{}, errAnalysis("Unknown tokenizer type [%s] for [%s]", typ, name)
	}
	tok, err := f(&compBuild{ctx: ctx, name: name, kind: "tokenizer", s: s})
	return namedTokenizer{name: name, tok: tok}, err
}

// tokenFilter builds a token filter; chain is the analyzer up to it.
func (ctx *anContext) tokenFilter(nod any, chain *anAnalyzer) (namedTokenFilter, bool, error) {
	name, typ, s, err := ctx.definition("filter", nod)
	if err != nil {
		return namedTokenFilter{}, false, err
	}
	def, ok := filterTypes[typ]
	if !ok {
		return namedTokenFilter{}, false, errAnalysis("Unknown filter type [%s] for [%s]", typ, name)
	}
	if ctx.depth > 8 {
		return namedTokenFilter{}, false, errAnalysis("filter [%s] refers to itself", name)
	}
	ctx.depth++
	defer func() { ctx.depth-- }()
	tf, err := def.build(&compBuild{ctx: ctx, name: name, kind: "filter", s: s, chain: chain})
	return namedTokenFilter{name: name, tf: tf, attrs: def.attrs}, def.normalizing, err
}

func knownComponentType(kind, typ string) bool {
	if japaneseAnalyzerType == "" && kuromojiComponent(kind, typ) {
		return false
	}
	switch kind {
	case "tokenizer":
		_, ok := tokenizerTypes[typ]
		return ok
	case "char_filter":
		_, ok := charFilterTypes[typ]
		return ok
	case "filter":
		_, ok := filterTypes[typ]
		return ok
	}
	return false
}

// customChain builds the analyzer of an _analyze request that defines its
// own components (normalizer: no tokenizer, only normalizing filters).
func (ctx *anContext) customChain(tokenizer any, charFilters, filters []any) (*anAnalyzer, error) {
	a := &anAnalyzer{name: "__custom__", custom: true, posGap: 100, offsetGap: 1}
	normalizer := tokenizer == nil
	for _, nod := range charFilters {
		cf, err := ctx.charFilter(nod)
		if err != nil {
			return nil, err
		}
		a.charFilters = append(a.charFilters, cf)
	}
	if normalizer {
		a.tokenizer = namedTokenizer{name: "keyword", tok: keywordTokenizer{}}
	} else {
		tok, err := ctx.tokenizer(tokenizer)
		if err != nil {
			return nil, err
		}
		a.tokenizer = tok
	}
	for _, nod := range filters {
		chain := &anAnalyzer{charFilters: a.charFilters, tokenizer: a.tokenizer, filters: append([]namedTokenFilter(nil), a.filters...)}
		tf, normalizing, err := ctx.tokenFilter(nod, chain)
		if err != nil {
			return nil, err
		}
		if normalizer && !normalizing {
			return nil, errAnalysis("Custom normalizer may not use filter [%s]", tf.name)
		}
		a.filters = append(a.filters, tf)
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// component factories: tokenizers and char filters
// ---------------------------------------------------------------------------

var (
	tokenizerTypes  map[string]func(*compBuild) (anTokenizer, error)
	charFilterTypes map[string]func(*compBuild) (anCharFilter, error)
)

func scannerMaxTokenLength(b *compBuild) (int, error) {
	n, err := b.int("max_token_length", 255)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		return 0, errCreateTime("maxTokenLength must be greater than zero")
	}
	if n > 1048576 {
		return 0, errCreateTime("maxTokenLength may not exceed 1048576")
	}
	return n, nil
}

func charTokenizerMaxLength(b *compBuild) (int, error) {
	n, err := b.int("max_token_length", 255)
	if err != nil {
		return 0, err
	}
	if n <= 0 || n > 1048576 {
		return 0, errCreateTime("maxTokenLen must be greater than 0 and less than 1048576 passed: %d", n)
	}
	return n, nil
}

var tokenCharClasses = map[string]func(rune) bool{
	"letter": unicode.IsLetter, "digit": func(r rune) bool { return unicode.Is(unicode.Nd, r) }, "whitespace": javaIsWhitespace,
	"punctuation": javaIsPunctuation, "symbol": javaIsSymbol,
}

func init() {
	cats := map[string]*unicode.RangeTable{
		"uppercase_letter": unicode.Lu, "lowercase_letter": unicode.Ll, "titlecase_letter": unicode.Lt, "modifier_letter": unicode.Lm,
		"other_letter": unicode.Lo, "non_spacing_mark": unicode.Mn, "enclosing_mark": unicode.Me, "combining_spacing_mark": unicode.Mc,
		"decimal_digit_number": unicode.Nd, "letter_number": unicode.Nl, "other_number": unicode.No, "space_separator": unicode.Zs,
		"line_separator": unicode.Zl, "paragraph_separator": unicode.Zp, "control": unicode.Cc, "format": unicode.Cf,
		"private_use": unicode.Co, "surrogate": unicode.Cs, "dash_punctuation": unicode.Pd, "start_punctuation": unicode.Ps,
		"end_punctuation": unicode.Pe, "connector_punctuation": unicode.Pc, "other_punctuation": unicode.Po, "math_symbol": unicode.Sm,
		"currency_symbol": unicode.Sc, "modifier_symbol": unicode.Sk, "other_symbol": unicode.So,
		"initial_quote_punctuation": unicode.Pi, "final_quote_punctuation": unicode.Pf,
	}
	for name, table := range cats {
		t := table
		tokenCharClasses[name] = func(r rune) bool { return unicode.Is(t, r) }
	}
	tokenCharClasses["unassigned"] = func(r rune) bool {
		for _, t := range unicode.Categories {
			if unicode.Is(t, r) {
				return false
			}
		}
		return true
	}
}

const tokenCharsNames = "symbol, private_use, paragraph_separator, start_punctuation, unassigned, enclosing_mark, connector_punctuation, letter_number, other_number, math_symbol, lowercase_letter, space_separator, surrogate, initial_quote_punctuation, decimal_digit_number, digit, other_punctuation, dash_punctuation, currency_symbol, non_spacing_mark, custom, format, modifier_letter, control, uppercase_letter, other_symbol, end_punctuation, modifier_symbol, other_letter, line_separator, titlecase_letter, letter, punctuation, combining_spacing_mark, final_quote_punctuation, whitespace"

func ngramTokenizerFactory(edge bool) func(*compBuild) (anTokenizer, error) {
	return func(b *compBuild) (anTokenizer, error) {
		minGram, err := b.int("min_gram", 1)
		if err != nil {
			return nil, err
		}
		maxGram, err := b.int("max_gram", 2)
		if err != nil {
			return nil, err
		}
		if !edge && maxGram-minGram > b.ctx.maxNgramDiff {
			return nil, errAnalysis("The difference between max_gram and min_gram in NGram Tokenizer must be less than or equal to: [%d] but was [%d]. This limit can be set by changing the [index.max_ngram_diff] index level setting.", b.ctx.maxNgramDiff, maxGram-minGram)
		}
		if minGram < 1 {
			return nil, errCreateTime("minGram must be greater than zero")
		}
		if minGram > maxGram {
			return nil, errCreateTime("minGram must not be greater than maxGram")
		}
		t := ngramTokenizer{minGram: minGram, maxGram: maxGram, edgesOnly: edge}
		classes, _ := b.list("token_chars")
		var matchers []func(rune) bool
		for _, c := range classes {
			c = strings.TrimSpace(strings.ToLower(c))
			if c == "custom" {
				custom := b.str("custom_token_chars", "")
				if custom == "" {
					return nil, errAnalysis("Token type: 'custom' requires setting `custom_token_chars`")
				}
				matchers = append(matchers, func(r rune) bool { return strings.ContainsRune(custom, r) })
				continue
			}
			m, ok := tokenCharClasses[c]
			if !ok {
				return nil, errAnalysis("Unknown token type: '%s', must be one of [%s]", c, tokenCharsNames)
			}
			matchers = append(matchers, m)
		}
		if len(matchers) > 0 {
			t.isTokenChar = func(r rune) bool {
				for _, m := range matchers {
					if m(r) {
						return true
					}
				}
				return false
			}
		}
		return t, nil
	}
}

// parseCharGroupChar is CharGroupTokenizerFactory.parseEscapedChar.
func parseCharGroupChar(s string) (rune, error) {
	fail := &Error{Status: 500, Type: "runtime_exception", Reason: "Invalid escaped char in [" + s + "]"}
	if len(s) < 2 || s[0] != '\\' {
		return 0, fail
	}
	switch s[1] {
	case '\\', '\'', '"':
		return rune(s[1]), nil
	case 'n':
		return '\n', nil
	case 't':
		return '\t', nil
	case 'r':
		return '\r', nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'u':
		if len(s) > 6 {
			return 0, fail
		}
		v, err := strconv.ParseUint(s[2:], 16, 16)
		if err != nil {
			return 0, &Error{Status: 500, Type: "number_format_exception", Reason: fmt.Sprintf("For input string: \"%s\"", s[2:])}
		}
		return rune(v), nil
	}
	return 0, &Error{Status: 500, Type: "runtime_exception", Reason: fmt.Sprintf("Invalid escaped char %c in [%s]", s[1], s)}
}

func charGroupTokenizerFactory(b *compBuild) (anTokenizer, error) {
	maxLen, err := charTokenizerMaxLength(b)
	if err != nil {
		return nil, err
	}
	var space, letter, digit, punct, symbol bool
	chars := map[rune]bool{}
	items, _ := b.list("tokenize_on_chars")
	for _, c := range items {
		switch {
		case c == "":
			return nil, &Error{Status: 500, Type: "runtime_exception", Reason: "[tokenize_on_chars] cannot contain empty characters"}
		case utf8.RuneCountInString(c) == 1:
			r, _ := utf8.DecodeRuneInString(c)
			chars[r] = true
		case c[0] == '\\':
			r, err := parseCharGroupChar(c)
			if err != nil {
				return nil, err
			}
			chars[r] = true
		case c == "letter":
			letter = true
		case c == "digit":
			digit = true
		case c == "whitespace":
			space = true
		case c == "punctuation":
			punct = true
		case c == "symbol":
			symbol = true
		default:
			return nil, &Error{Status: 500, Type: "runtime_exception", Reason: "Invalid escaped char in [" + c + "]"}
		}
	}
	return charTokenizer{maxLen: maxLen, isTokenChar: func(r rune) bool {
		switch {
		case space && javaIsWhitespace(r), letter && unicode.IsLetter(r), digit && unicode.Is(unicode.Nd, r),
			punct && javaIsPunctuation(r), symbol && javaIsSymbol(r):
			return false
		}
		return !chars[r]
	}}, nil
}

func pathHierarchyFactory(b *compBuild) (anTokenizer, error) {
	delim := b.str("delimiter", "/")
	if utf8.RuneCountInString(delim) != 1 {
		return nil, errAnalysis("delimiter must be a one char value")
	}
	repl := b.str("replacement", delim)
	if utf8.RuneCountInString(repl) != 1 {
		return nil, errAnalysis("replacement must be a one char value")
	}
	skip, err := b.int("skip", 0)
	if err != nil {
		return nil, err
	}
	if skip < 0 {
		return nil, errAnalysis("skip can't be negative")
	}
	reverse, err := b.bool("reverse", false)
	if err != nil {
		return nil, err
	}
	d, _ := utf8.DecodeRuneInString(delim)
	r, _ := utf8.DecodeRuneInString(repl)
	return pathHierarchyTokenizer{delimiter: uint16(d), replacement: uint16(r), skip: skip, reverse: reverse}, nil
}

// compileJavaRegex compiles a java.util.regex pattern with OpenSearch regex
// flags, rejecting syntax Go's regexp cannot run.
func compileJavaRegex(pattern, flags string) (*regexp.Regexp, error) {
	var mods string
	literal, comments := false, false
	for _, f := range strings.Split(flags, "|") {
		if f == "" {
			continue
		}
		switch f = strings.ToUpper(f); f {
		case "CASE_INSENSITIVE":
			mods += "i"
		case "MULTILINE":
			mods += "m"
		case "DOTALL":
			mods += "s"
		case "LITERAL":
			literal = true
		case "COMMENTS":
			comments = true
		case "CANON_EQ", "UNICODE_CASE", "UNICODE_CHARACTER_CLASS", "UNIX_LINES":
		default:
			return nil, errAnalysis("Unknown regex flag [%s]", f)
		}
	}
	expr := pattern
	if literal {
		expr = regexp.QuoteMeta(pattern)
	} else {
		if comments {
			expr = stripRegexComments(expr)
		}
		for _, bad := range []string{"(?=", "(?!", "(?<=", "(?<!", "(?>", "*+", "++", "?+", "}+", "\\p{java", "\\p{Is", "\\p{In", "\\h", "\\H", "\\R", "\\X", "\\G", "\\Z", "\\k<", "&&"} {
			if strings.Contains(expr, bad) {
				return nil, unsupported("the regular expression [%s]", pattern)
			}
		}
		for i := 0; i+1 < len(expr); i++ {
			if expr[i] == '\\' {
				if expr[i+1] >= '1' && expr[i+1] <= '9' {
					return nil, unsupported("the regular expression [%s]", pattern)
				}
				i++
			}
		}
	}
	if mods != "" {
		expr = "(?" + mods + ")" + expr
	}
	re, err := regexp.Compile(expr)
	if err == nil {
		return re, nil
	}
	var se *syntax.Error
	if errors.As(err, &se) {
		switch se.Code {
		case syntax.ErrMissingParen:
			return nil, patternSyntaxError("Unclosed group", pattern, len(pattern))
		case syntax.ErrMissingBracket:
			return nil, patternSyntaxError("Unclosed character class", pattern, len(pattern)-1)
		case syntax.ErrMissingRepeatArgument:
			op := strings.Trim(se.Expr, "`")
			idx := strings.Index(pattern, op)
			if idx < 0 {
				idx = 0
			}
			return nil, patternSyntaxError("Dangling meta character '"+op[:1]+"'", pattern, idx)
		case syntax.ErrUnexpectedParen:
			return nil, patternSyntaxError("Unmatched closing ')'", pattern, strings.Index(pattern, ")")-1)
		}
	}
	return nil, unsupported("the regular expression [%s]", pattern)
}

func patternSyntaxError(desc, pattern string, index int) *Error {
	msg := desc
	if index >= 0 {
		msg += fmt.Sprintf(" near index %d", index)
	}
	msg += "\n" + pattern
	if index >= 0 && index < len(pattern) {
		msg += "\n" + strings.Repeat(" ", index) + "^"
	}
	return &Error{Status: 400, Type: "pattern_syntax_exception", Reason: msg}
}

func stripRegexComments(p string) string {
	var sb strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			sb.WriteByte(c)
			i++
			sb.WriteByte(p[i])
			continue
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case !inClass && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'):
			continue
		case !inClass && c == '#':
			for i < len(p) && p[i] != '\n' {
				i++
			}
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

// luceneRegexpToGo translates Lucene RegExp syntax (simple_pattern).
func luceneRegexpToGo(p string) (string, error) {
	var sb strings.Builder
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case '\\':
			if i+1 < len(rs) {
				i++
				switch rs[i] {
				case 'd', 'D', 's', 'S', 'w', 'W':
					sb.WriteRune('\\')
					sb.WriteRune(rs[i])
				default:
					sb.WriteString(regexp.QuoteMeta(string(rs[i])))
				}
			}
		case '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			if j >= len(rs) {
				return "", errAnalysis("expected '\"' at position %d", len(rs))
			}
			sb.WriteString(regexp.QuoteMeta(string(rs[i+1 : j])))
			i = j
		case '@':
			sb.WriteString("(?s:.*)")
		case '.':
			sb.WriteString("(?s:.)")
		case '#':
			sb.WriteString(`[^\x00-\x{10FFFF}]`)
		case '&', '~', '<':
			return "", unsupported("the simple pattern [%s]", p)
		case '^', '$':
			sb.WriteRune('\\')
			sb.WriteRune(c)
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String(), nil
}

func simplePatternFactory(split bool) func(*compBuild) (anTokenizer, error) {
	return func(b *compBuild) (anTokenizer, error) {
		expr, err := luceneRegexpToGo(b.str("pattern", ""))
		if err != nil {
			return nil, err
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, errAnalysis("invalid pattern [%s]", b.str("pattern", ""))
		}
		re.Longest()
		return simplePatternTokenizer{re: re, split: split}, nil
	}
}

// parseMappingString is MappingCharFilterFactory.parseString.
func parseMappingString(s string) (string, error) {
	var out []rune
	rs := []rune(s)
	for i := 0; i < len(rs); {
		c := rs[i]
		i++
		if c == '\\' {
			if i >= len(rs) {
				return "", fmt.Errorf("Invalid escaped char in [%s]", s)
			}
			c = rs[i]
			i++
			switch c {
			case 'n':
				c = '\n'
			case 't':
				c = '\t'
			case 'r':
				c = '\r'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'u':
				if i+3 >= len(rs) {
					return "", fmt.Errorf("Invalid escaped char in [%s]", s)
				}
				hex := string(rs[i : i+4])
				v, err := strconv.ParseUint(hex, 16, 16)
				if err != nil {
					return "", fmt.Errorf("For input string: \"%s\" under radix 16", hex)
				}
				c = rune(v)
				i += 4
			}
		}
		out = append(out, c)
	}
	return string(out), nil
}

func mappingCharFilterFactory(b *compBuild) (anCharFilter, error) {
	rules, ok := b.list("mappings")
	if raw, has := b.raw("mappings"); has {
		if arr, isArr := raw.([]any); isArr {
			rules, ok = nil, true
			for _, e := range arr {
				rules = append(rules, analysisSettingString(e))
			}
		} else {
			rules, ok = []string{analysisSettingString(raw)}, true
		}
	}
	if !ok {
		if _, has := b.raw("mappings_path"); has {
			return nil, unsupported("[mappings_path] of char filter [%s]", b.name)
		}
		return nil, errAnalysis("mapping requires either `mappings` or `mappings_path` to be configured")
	}
	f := &mappingCharFilter{rules: map[string]string{}}
	for n, rule := range rules {
		i := strings.LastIndex(rule, "=>")
		if i < 0 {
			return nil, &Error{Status: 500, Type: "runtime_exception", Reason: fmt.Sprintf("Line [%d]: Invalid mapping rule : [%s]", n+1, rule)}
		}
		lhs, err := parseMappingString(javaTrimString(rule[:i]))
		if err == nil {
			var rhs string
			rhs, err = parseMappingString(javaTrimString(rule[i+2:]))
			if err == nil {
				if lhs == "" {
					return nil, errAnalysis("cannot match the empty string")
				}
				if _, dup := f.rules[lhs]; dup {
					return nil, errAnalysis("match \"%s\" was already added", lhs)
				}
				f.rules[lhs] = rhs
				if l := utf16Len(lhs); l > f.maxLen {
					f.maxLen = l
				}
				continue
			}
		}
		return nil, &Error{Status: 500, Type: "runtime_exception", Reason: fmt.Sprintf("Line [%d]: %s", n+1, err.Error())}
	}
	return f, nil
}

// javaTrimString is String.trim (removes code points <= U+0020).
func javaTrimString(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

func patternReplaceCharFilterFactory(b *compBuild) (anCharFilter, error) {
	pattern, ok := b.raw("pattern")
	if !ok || analysisSettingString(pattern) == "" {
		return nil, errAnalysis("pattern is missing for [%s] char filter of type 'pattern_replace'", b.name)
	}
	re, err := compileJavaRegex(analysisSettingString(pattern), b.str("flags", ""))
	if err != nil {
		return nil, err
	}
	repl := b.str("replacement", "")
	if err := checkJavaReplacement(repl, re.NumSubexp()); err != nil {
		return nil, err
	}
	return &patternReplaceCharFilter{re: re, replacement: repl}, nil
}

func init() {
	charFilterTypes = map[string]func(*compBuild) (anCharFilter, error){
		"html_strip": func(b *compBuild) (anCharFilter, error) {
			tags, _ := b.list("escaped_tags")
			f := &htmlStripCharFilter{escaped: map[string]bool{}}
			for _, t := range tags {
				f.escaped[strings.ToLower(t)] = true
			}
			return f, nil
		},
		"mapping":         mappingCharFilterFactory,
		"pattern_replace": patternReplaceCharFilterFactory,
	}
	tokenizerTypes = map[string]func(*compBuild) (anTokenizer, error){
		"standard": func(b *compBuild) (anTokenizer, error) {
			n, err := scannerMaxTokenLength(b)
			return standardTokenizer{maxLen: n}, err
		},
		"classic": func(b *compBuild) (anTokenizer, error) {
			n, err := scannerMaxTokenLength(b)
			return classicTokenizer{maxLen: n}, err
		},
		"uax_url_email": func(b *compBuild) (anTokenizer, error) {
			n, err := scannerMaxTokenLength(b)
			return uaxURLEmailTokenizer{maxLen: n}, err
		},
		"whitespace": func(b *compBuild) (anTokenizer, error) {
			n, err := charTokenizerMaxLength(b)
			return charTokenizer{isTokenChar: func(r rune) bool { return !javaIsWhitespace(r) }, maxLen: n}, err
		},
		"letter": func(b *compBuild) (anTokenizer, error) {
			return charTokenizer{isTokenChar: unicode.IsLetter, maxLen: 255}, nil
		},
		"lowercase": func(b *compBuild) (anTokenizer, error) {
			return charTokenizer{isTokenChar: unicode.IsLetter, normalize: javaToLower, maxLen: 255}, nil
		},
		"keyword": func(b *compBuild) (anTokenizer, error) {
			if n, err := b.int("buffer_size", 256); err != nil {
				return nil, err
			} else if n <= 0 {
				return nil, errCreateTime("bufferSize must be > 0")
			}
			return keywordTokenizer{}, nil
		},
		"ngram":      ngramTokenizerFactory(false),
		"edge_ngram": ngramTokenizerFactory(true),
		"pattern": func(b *compBuild) (anTokenizer, error) {
			re, err := compileJavaRegex(b.str("pattern", `\W+`), b.str("flags", ""))
			if err != nil {
				return nil, err
			}
			group, err := b.int("group", -1)
			return patternTokenizer{re: re, group: group}, err
		},
		"simple_pattern":       simplePatternFactory(false),
		"simple_pattern_split": simplePatternFactory(true),
		"char_group":           charGroupTokenizerFactory,
		"path_hierarchy":       pathHierarchyFactory,
		"PathHierarchy":        pathHierarchyFactory,
		"thai": func(b *compBuild) (anTokenizer, error) {
			return nil, unsupported("the [thai] tokenizer")
		},
	}
}

// ---------------------------------------------------------------------------
// component factories: token filters
// ---------------------------------------------------------------------------

type filterDef struct {
	build       func(*compBuild) (anTokenFilter, error)
	attrs       int
	normalizing bool
}

var filterTypes map[string]filterDef

// words reads a word list setting (Analysis.parseWords); named resolves
// _lang_ stop word sets.
func (b *compBuild) words(key string, def []string, ignoreCase, named bool) (*wordSet, bool, error) {
	raw, has := b.raw(key)
	if !has {
		if _, p := b.raw(key + "_path"); p {
			return nil, false, unsupported("[%s_path] of %s [%s]", key, componentKindName(b.kind), b.name)
		}
		if def == nil {
			return nil, false, nil
		}
		return newWordSet(def, ignoreCase), false, nil
	}
	if s, ok := raw.(string); ok && s == "_none_" {
		return newWordSet(nil, ignoreCase), true, nil
	}
	list, _ := b.list(key)
	set := newWordSet(nil, ignoreCase)
	for _, w := range list {
		if named {
			if words, known, err := namedStopWords(w); known {
				if err != nil {
					return nil, true, err
				}
				for _, n := range words {
					set.add(n)
				}
				continue
			}
		}
		set.add(w)
	}
	return set, true, nil
}

func simpleFilter(f func(t *anToken)) func(*compBuild) (anTokenFilter, error) {
	return func(*compBuild) (anTokenFilter, error) { return mapTerms(f), nil }
}

func unsupportedFilter(b *compBuild) (anTokenFilter, error) {
	return nil, unsupported("the [%s] token filter", filterTypeOf(b))
}

func filterTypeOf(b *compBuild) string {
	if strings.HasPrefix(b.name, "__anonymous__") {
		return strings.TrimPrefix(b.name, "__anonymous__")
	}
	if def, ok := getMap(b.ctx.analysis, "filter")[b.name].(M); ok && b.ctx.hasIndex {
		return getString(def, "type")
	}
	return b.name
}

func stemmerFilterFactory(snowball bool) func(*compBuild) (anTokenFilter, error) {
	return func(b *compBuild) (anTokenFilter, error) {
		var stem func(string) string
		var ok bool
		var err error
		if snowball {
			stem, ok, err = snowballForLanguage(b.str("language", "English"))
		} else {
			stem, ok, err = stemmerForLanguage(b.str("language", b.str("name", "porter")))
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, unsupported("the stemmer language [%s]", b.str("language", b.str("name", "")))
		}
		return mapTerms(stemFilter(stem)), nil
	}
}

func ngramFilterFactory(edge bool) func(*compBuild) (anTokenFilter, error) {
	return func(b *compBuild) (anTokenFilter, error) {
		minGram, err := b.int("min_gram", 1)
		if err != nil {
			return nil, err
		}
		maxGram, err := b.int("max_gram", 2)
		if err != nil {
			return nil, err
		}
		preserve, err := b.bool("preserve_original", false)
		if err != nil {
			return nil, err
		}
		if !edge && maxGram-minGram > b.ctx.maxNgramDiff {
			return nil, errAnalysis("The difference between max_gram and min_gram in NGram Tokenizer must be less than or equal to: [%d] but was [%d]. This limit can be set by changing the [index.max_ngram_diff] index level setting.", b.ctx.maxNgramDiff, maxGram-minGram)
		}
		if minGram < 1 {
			return nil, errCreateTime("minGram must be greater than zero")
		}
		if minGram > maxGram {
			return nil, errCreateTime("minGram must not be greater than maxGram")
		}
		if edge {
			return edgeNGramTokenFilter(minGram, maxGram, preserve, b.str("side", "front") == "back"), nil
		}
		return ngramTokenFilter(minGram, maxGram, preserve), nil
	}
}

func shingleFilterFactory(b *compBuild) (anTokenFilter, error) {
	maxSize, err := b.int("max_shingle_size", 2)
	if err != nil {
		return nil, err
	}
	minSize, err := b.int("min_shingle_size", 2)
	if err != nil {
		return nil, err
	}
	unigrams, err := b.bool("output_unigrams", true)
	if err != nil {
		return nil, err
	}
	ifNo, err := b.bool("output_unigrams_if_no_shingles", false)
	if err != nil {
		return nil, err
	}
	diff := maxSize - minSize
	if unigrams {
		diff++
	}
	if diff > b.ctx.maxShingleDiff {
		return nil, errAnalysis("In Shingle TokenFilter the difference between max_shingle_size and min_shingle_size (and +1 if outputting unigrams) must be less than or equal to: [%d] but was [%d]. This limit can be set by changing the [index.max_shingle_diff] index level setting.", b.ctx.maxShingleDiff, diff)
	}
	switch {
	case maxSize < 2:
		return nil, errCreateTime("Max shingle size must be >= 2")
	case minSize < 2:
		return nil, errCreateTime("Min shingle size must be >= 2")
	case minSize > maxSize:
		return nil, errCreateTime("Min shingle size must be <= max shingle size")
	}
	return shingleFilter{min: minSize, max: maxSize, outputUnigrams: unigrams, unigramsIfNoShingles: ifNo,
		separator: b.str("token_separator", " "), filler: b.str("filler_token", "_")}, nil
}

func synonymFilterFactory(graph bool) func(*compBuild) (anTokenFilter, error) {
	return func(b *compBuild) (anTokenFilter, error) {
		typ := "synonym"
		if graph {
			typ = "synonym_graph"
		}
		rules, ok := b.list("synonyms")
		if raw, has := b.raw("synonyms"); has {
			if arr, isArr := raw.([]any); isArr {
				rules = nil
				for _, e := range arr {
					rules = append(rules, analysisSettingString(e))
				}
			} else {
				rules = []string{analysisSettingString(raw)}
			}
			ok = true
		}
		if !ok {
			if _, has := b.raw("synonyms_path"); has {
				return nil, unsupported("[synonyms_path] of token filter [%s]", b.name)
			}
			return nil, errAnalysis("%s requires either `synonyms` or `synonyms_path` to be configured", typ)
		}
		if f := b.str("format", ""); f == "wordnet" {
			return nil, unsupported("the wordnet synonym format")
		}
		expand, err := b.bool("expand", true)
		if err != nil {
			return nil, err
		}
		lenient, err := b.bool("lenient", false)
		if err != nil {
			return nil, err
		}
		chain := b.chain
		if chain == nil {
			chain = &anAnalyzer{tokenizer: namedTokenizer{name: "standard", tok: standardTokenizer{maxLen: 255}}}
		}
		m, err := parseSolrSynonyms(rules, expand, lenient, chain)
		if err != nil {
			return nil, err
		}
		if graph {
			return synonymGraphFilter(m), nil
		}
		return synonymFilter(m), nil
	}
}

func wordDelimiterFactory(graph bool) func(*compBuild) (anTokenFilter, error) {
	return func(b *compBuild) (anTokenFilter, error) {
		o := &wordDelimiterOptions{}
		for _, opt := range []struct {
			key string
			dst *bool
			def bool
		}{
			{"generate_word_parts", &o.generateWordParts, true}, {"generate_number_parts", &o.generateNumberParts, true},
			{"catenate_words", &o.catenateWords, false}, {"catenate_numbers", &o.catenateNumbers, false}, {"catenate_all", &o.catenateAll, false},
			{"split_on_case_change", &o.splitOnCaseChange, true}, {"preserve_original", &o.preserveOriginal, false},
			{"split_on_numerics", &o.splitOnNumerics, true}, {"stem_english_possessive", &o.stemEnglishPossessive, true},
			{"adjust_offsets", &o.adjustOffsets, true}, {"ignore_keywords", &o.ignoreKeywords, false},
		} {
			v, err := b.bool(opt.key, opt.def)
			if err != nil {
				return nil, err
			}
			*opt.dst = v
		}
		if words, ok := b.list("protected_words"); ok {
			o.protected = newWordSet(words, false)
		}
		if rules, ok := b.list("type_table"); ok {
			o.types = map[uint16]byte{}
			names := map[string]byte{"LOWER": wdLower, "UPPER": wdUpper, "ALPHA": wdAlpha, "DIGIT": wdDigit, "ALPHANUM": wdAlphaNum, "SUBWORD_DELIM": wdSubwordDelim}
			for _, rule := range rules {
				i := strings.LastIndex(rule, "=>")
				if i < 0 {
					return nil, &Error{Status: 500, Type: "runtime_exception", Reason: "Invalid Mapping Rule : [" + rule + "]"}
				}
				lhs, err := parseMappingString(javaTrimString(rule[:i]))
				if err != nil || utf16Len(lhs) != 1 {
					return nil, &Error{Status: 500, Type: "runtime_exception", Reason: "Invalid Mapping Rule : [" + rule + "]. Only a single character is allowed."}
				}
				typ, ok := names[javaTrimString(rule[i+2:])]
				if !ok {
					return nil, &Error{Status: 500, Type: "runtime_exception", Reason: "Invalid Mapping Rule : [" + rule + "]. Illegal type."}
				}
				o.types[utf16.Encode([]rune(lhs))[0]] = typ
			}
		}
		if graph {
			return wordDelimiterGraphFilter(o), nil
		}
		return wordDelimiterFilter(o), nil
	}
}

func multiplexerFactory(b *compBuild) (anTokenFilter, error) {
	preserve, err := b.bool("preserve_original", true)
	if err != nil {
		return nil, err
	}
	var chains [][]namedTokenFilter
	raw, _ := b.raw("filters")
	var items []string
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			items = append(items, analysisSettingString(e))
		}
	} else if raw != nil {
		items = []string{analysisSettingString(raw)}
	}
	for _, item := range items {
		var chain []namedTokenFilter
		for _, name := range strings.Split(item, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			tf, _, err := b.ctx.tokenFilter(name, b.chain)
			if err != nil {
				var e *Error
				if errors.As(err, &e) && strings.HasPrefix(e.Reason, "failed to find") {
					return nil, errAnalysis("Multiplexing filter [%s] refers to undefined tokenfilter [%s]", b.name, name)
				}
				return nil, err
			}
			chain = append(chain, tf)
		}
		chains = append(chains, chain)
	}
	return tokenFunc(func(in anStream) anStream {
		out := anStream{finalOffset: in.finalOffset, finalPosInc: in.finalPosInc}
		for _, t := range in.tokens {
			start := len(out.tokens)
			if preserve {
				out.tokens = append(out.tokens, t)
			}
			for _, chain := range chains {
				s := anStream{tokens: []anToken{t}, finalOffset: in.finalOffset}
				for _, f := range chain {
					s = f.tf.apply(s)
				}
				for _, k := range s.tokens {
					if len(out.tokens) > start {
						k.posInc = 0
					} else {
						k.posInc = t.posInc
					}
					dup := false
					for _, prev := range out.tokens[start:] {
						if prev.term == k.term {
							dup = true
							break
						}
					}
					if !dup {
						out.tokens = append(out.tokens, k)
					}
				}
			}
		}
		return out
	}), nil
}

func bleveFilterFactory(f analysis.TokenFilter) func(*compBuild) (anTokenFilter, error) {
	return simpleFilter(func(t *anToken) { t.term = bleveTermFilter(f)(t.term) })
}

func init() {
	filterTypes = map[string]filterDef{
		"lowercase": {normalizing: true, build: func(b *compBuild) (anTokenFilter, error) {
			lang, has := b.raw("language")
			if !has {
				return mapTerms(lowercaseFilter), nil
			}
			switch l := analysisSettingString(lang); {
			case strings.EqualFold(l, "greek"):
				return mapTerms(func(t *anToken) { t.term = strings.Map(greekLowercase, t.term) }), nil
			case strings.EqualFold(l, "irish"):
				return mapTerms(irishLowercaseFilter), nil
			case strings.EqualFold(l, "turkish"):
				return mapTerms(turkishLowercaseFilter), nil
			default:
				return nil, errAnalysis("language [%s] not support for lower case", l)
			}
		}},
		"uppercase": {normalizing: true, build: simpleFilter(uppercaseFilter)},
		"asciifolding": {normalizing: true, build: func(b *compBuild) (anTokenFilter, error) {
			p, err := b.bool("preserve_original", false)
			return asciiFoldingChainFilter(p), err
		}},
		"stop": {build: func(b *compBuild) (anTokenFilter, error) {
			ignoreCase, err := b.bool("ignore_case", false)
			if err != nil {
				return nil, err
			}
			words, _, err := b.words("stopwords", englishStopWords, ignoreCase, true)
			if err != nil {
				return nil, err
			}
			removeTrailing, err := b.bool("remove_trailing", true)
			return stopTokenFilter(words, removeTrailing), err
		}},
		"stemmer":              {attrs: attrKeyword, build: stemmerFilterFactory(false)},
		"porter_stem":          {attrs: attrKeyword, build: simpleFilter(stemFilter(porterStem))},
		"snowball":             {attrs: attrKeyword, build: stemmerFilterFactory(true)},
		"kstem":                {attrs: attrKeyword, build: unsupportedFilter},
		"shingle":              {build: shingleFilterFactory},
		"synonym":              {build: synonymFilterFactory(false)},
		"synonym_graph":        {build: synonymFilterFactory(true)},
		"word_delimiter":       {attrs: attrKeyword, build: wordDelimiterFactory(false)},
		"word_delimiter_graph": {attrs: attrKeyword, build: wordDelimiterFactory(true)},
		"ngram":                {build: ngramFilterFactory(false)},
		"edge_ngram":           {build: ngramFilterFactory(true)},
		"trim":                 {normalizing: true, build: simpleFilter(func(t *anToken) { t.term = javaTrimWhitespace(t.term) })},
		"truncate": {attrs: attrKeyword, build: func(b *compBuild) (anTokenFilter, error) {
			n := 10
			if _, has := b.raw("length"); has || b.name != "truncate" {
				var err error
				if n, err = b.int("length", -1); err != nil {
					return nil, err
				}
				if n <= 0 {
					return nil, errAnalysis("length parameter must be provided")
				}
			}
			return mapTerms(func(t *anToken) {
				if !t.keyword {
					t.term = truncateUTF16(t.term, n)
				}
			}), nil
		}},
		"length": {build: func(b *compBuild) (anTokenFilter, error) {
			minLen, err := b.int("min", 0)
			if err != nil {
				return nil, err
			}
			maxLen, err := b.int("max", math.MaxInt32)
			if err != nil {
				return nil, err
			}
			return filtering(func(t *anToken) bool { l := utf16Len(t.term); return l >= minLen && l <= maxLen }), nil
		}},
		"unique": {build: func(b *compBuild) (anTokenFilter, error) {
			same, err := b.bool("only_on_same_position", false)
			return uniqueTokenFilter(same), err
		}},
		"reverse": {build: simpleFilter(func(t *anToken) { t.term = reverseRunes(t.term) })},
		"elision": {normalizing: true, build: func(b *compBuild) (anTokenFilter, error) {
			articlesCase, err := b.bool("articles_case", false)
			if err != nil {
				return nil, err
			}
			articles, has, err := b.words("articles", nil, articlesCase, false)
			if err != nil {
				return nil, err
			}
			if !has {
				articles = newWordSet([]string{"l", "m", "t", "qu", "n", "s", "j", "d", "c", "jusqu", "quoiqu", "lorsqu", "puisqu"}, true)
			}
			return mapTerms(elisionFilter(articles)), nil
		}},
		"cjk_bigram": {build: func(b *compBuild) (anTokenFilter, error) {
			f := cjkBigramFilter{han: true, hiragana: true, katakana: true, hangul: true}
			scripts, _ := b.list("ignored_scripts")
			for _, s := range scripts {
				switch s {
				case "han":
					f.han = false
				case "hiragana":
					f.hiragana = false
				case "katakana":
					f.katakana = false
				case "hangul":
					f.hangul = false
				}
			}
			var err error
			f.outputUnigrams, err = b.bool("output_unigrams", false)
			return f, err
		}},
		"cjk_width":         {normalizing: true, build: func(*compBuild) (anTokenFilter, error) { return mapTerms(cjkWidthFilter()), nil }},
		"decimal_digit":     {normalizing: true, build: simpleFilter(decimalDigitFilter)},
		"keyword_repeat":    {attrs: attrKeyword, build: func(*compBuild) (anTokenFilter, error) { return keywordRepeatFilter, nil }},
		"remove_duplicates": {build: func(*compBuild) (anTokenFilter, error) { return removeDuplicatesFilter, nil }},
		"keyword_marker": {attrs: attrKeyword, build: func(b *compBuild) (anTokenFilter, error) {
			ignoreCase, err := b.bool("ignore_case", false)
			if err != nil {
				return nil, err
			}
			words, hasWords, err := b.words("keywords", nil, ignoreCase, false)
			if err != nil {
				return nil, err
			}
			pattern, hasPattern := b.raw("keywords_pattern")
			switch {
			case hasWords && hasPattern:
				return nil, errAnalysis("cannot specify both `keywords_pattern` and `keywords` or `keywords_path`")
			case hasPattern:
				re, err := compileJavaRegex(analysisSettingString(pattern), "")
				if err != nil {
					return nil, err
				}
				return mapTerms(keywordMarkerFilter(nil, re)), nil
			case hasWords:
				return mapTerms(keywordMarkerFilter(words, nil)), nil
			}
			return nil, errAnalysis("keyword filter requires either `keywords`, `keywords_path`, or `keywords_pattern` to be set")
		}},
		"stemmer_override": {attrs: attrKeyword, build: func(b *compBuild) (anTokenFilter, error) {
			rules, ok := b.list("rules")
			if raw, has := b.raw("rules"); has {
				if arr, isArr := raw.([]any); isArr {
					rules = nil
					for _, e := range arr {
						rules = append(rules, analysisSettingString(e))
					}
				}
			}
			if !ok {
				return nil, errAnalysis("stemmer_override requires either `rules` or `rules_path` to be configured")
			}
			m := map[string]string{}
			for _, rule := range rules {
				parts := strings.Split(rule, "=>")
				if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
					return nil, errAnalysis("Invalid Keyword override Rule:%s", rule)
				}
				for _, k := range strings.Split(parts[0], ",") {
					if k = strings.TrimSpace(k); k != "" {
						if _, dup := m[k]; !dup {
							m[k] = strings.TrimSpace(parts[1])
						}
					}
				}
			}
			return mapTerms(stemmerOverrideFilter(m)), nil
		}},
		"pattern_replace": {build: func(b *compBuild) (anTokenFilter, error) {
			pattern, ok := b.raw("pattern")
			if !ok {
				return nil, errAnalysis("pattern is missing for [%s] token filter of type 'pattern_replace'", b.name)
			}
			re, err := compileJavaRegex(analysisSettingString(pattern), b.str("flags", ""))
			if err != nil {
				return nil, err
			}
			repl := b.str("replacement", "")
			if err := checkJavaReplacement(repl, re.NumSubexp()); err != nil {
				return nil, err
			}
			all, err := b.bool("all", true)
			return mapTerms(patternReplaceTokenFilter(re, repl, all)), err
		}},
		"pattern_capture": {build: func(b *compBuild) (anTokenFilter, error) {
			patterns, ok := b.list("patterns")
			if raw, has := b.raw("patterns"); has {
				if arr, isArr := raw.([]any); isArr {
					patterns = nil
					for _, e := range arr {
						patterns = append(patterns, analysisSettingString(e))
					}
				}
			}
			if !ok {
				return nil, errAnalysis("required setting 'patterns' is missing for token filter [%s]", b.name)
			}
			var res []*regexp.Regexp
			for _, p := range patterns {
				re, err := compileJavaRegex(p, "")
				if err != nil {
					return nil, err
				}
				res = append(res, re)
			}
			preserve, err := b.bool("preserve_original", true)
			return patternCaptureFilter(res, preserve), err
		}},
		"apostrophe": {build: simpleFilter(apostropheFilter)},
		"classic":    {build: simpleFilter(classicFilter)},
		"fingerprint": {build: func(b *compBuild) (anTokenFilter, error) {
			sep := b.str("separator", " ")
			if utf16Len(sep) != 1 {
				return nil, errAnalysis("Setting [separator] must be a single, non-null character. [%s] was provided.", sep)
			}
			n, err := b.int("max_output_size", 255)
			return fingerprintFilter(sep, n), err
		}},
		"limit": {build: func(b *compBuild) (anTokenFilter, error) {
			n, err := b.int("max_token_count", 1)
			return limitFilter(n), err
		}},
		"keep": {build: func(b *compBuild) (anTokenFilter, error) {
			ignoreCase, err := b.bool("keep_words_case", false)
			if err != nil {
				return nil, err
			}
			words, has, err := b.words("keep_words", nil, ignoreCase, false)
			if err != nil {
				return nil, err
			}
			if !has {
				return nil, errAnalysis("keep requires `keep_words` or `keep_words_path` to be configured")
			}
			return keepWordsFilter(words), nil
		}},
		"keep_types": {build: func(b *compBuild) (anTokenFilter, error) {
			types, ok := b.list("types")
			if !ok {
				return nil, errAnalysis("keep_types requires `types` to be configured")
			}
			set := map[string]bool{}
			for _, t := range types {
				set[t] = true
			}
			mode := b.str("mode", "include")
			if mode != "include" && mode != "exclude" {
				return nil, errAnalysis("`mode` can only be include or exclude")
			}
			return keepTypesFilter(set, mode == "include"), nil
		}},
		"delimited_payload": {attrs: attrPayload, build: func(b *compBuild) (anTokenFilter, error) {
			delim := b.str("delimiter", "|")
			if utf8.RuneCountInString(delim) != 1 {
				return nil, errAnalysis("delimiter must be a one char value")
			}
			r, _ := utf8.DecodeRuneInString(delim)
			return mapTerms(delimitedPayloadFilter(r, b.str("encoding", "float"))), nil
		}},
		"type_as_payload": {attrs: attrPayload, build: simpleFilter(func(t *anToken) { t.payload = []byte(t.typ) })},
		"common_grams": {build: func(b *compBuild) (anTokenFilter, error) {
			ignoreCase, err := b.bool("ignore_case", false)
			if err != nil {
				return nil, err
			}
			words, has, err := b.words("common_words", nil, ignoreCase, false)
			if err != nil {
				return nil, err
			}
			if !has {
				return nil, errAnalysis("missing or empty [common_words] or [common_words_path] configuration for common_grams token filter")
			}
			query, err := b.bool("query_mode", false)
			return commonGramsFilter(words, query), err
		}},
		"flatten_graph": {build: func(*compBuild) (anTokenFilter, error) {
			return tokenFunc(func(in anStream) anStream { return in }), nil
		}},
		"multiplexer": {build: multiplexerFactory},
		"dictionary_decompounder": {build: func(b *compBuild) (anTokenFilter, error) {
			words, has, err := b.words("word_list", nil, false, false)
			if err != nil {
				return nil, err
			}
			if !has {
				return nil, errAnalysis("word_list must be provided for [%s], either as a path to a file, or directly", b.name)
			}
			minWord, _ := b.int("min_word_size", 5)
			minSub, _ := b.int("min_subword_size", 2)
			maxSub, _ := b.int("max_subword_size", 15)
			longest, err := b.bool("only_longest_match", false)
			return dictionaryDecompounder(words, minWord, minSub, maxSub, longest), err
		}},
		"hunspell": {build: func(b *compBuild) (anTokenFilter, error) {
			locale := b.str("locale", b.str("language", b.str("lang", "")))
			if locale == "" {
				return nil, errAnalysis("missing [locale | language | lang] configuration for hunspell token filter")
			}
			return nil, &Error{Status: 500, Type: "illegal_state_exception", Reason: "Failed to load hunspell dictionary for locale: " + locale}
		}},
		"german_normalization":       {normalizing: true, build: bleveFilterFactory(blevede.NewGermanNormalizeFilter())},
		"arabic_normalization":       {normalizing: true, build: bleveFilterFactory(blevear.NewArabicNormalizeFilter())},
		"persian_normalization":      {normalizing: true, build: bleveFilterFactory(blevefa.NewPersianNormalizeFilter())},
		"hindi_normalization":        {normalizing: true, build: bleveFilterFactory(blevehi.NewHindiNormalizeFilter())},
		"indic_normalization":        {normalizing: true, build: bleveFilterFactory(blevein.NewIndicNormalizeFilter())},
		"sorani_normalization":       {normalizing: true, build: bleveFilterFactory(bleveckb.NewSoraniNormalizeFilter())},
		"bengali_normalization":      {normalizing: true, build: unsupportedFilter},
		"scandinavian_folding":       {normalizing: true, build: unsupportedFilter},
		"scandinavian_normalization": {normalizing: true, build: unsupportedFilter},
		"serbian_normalization":      {normalizing: true, build: unsupportedFilter},
		"persian_stem":               {normalizing: true, attrs: attrKeyword, build: unsupportedFilter},
	}
	for _, name := range []string{"arabic_stem", "brazilian_stem", "czech_stem", "dutch_stem", "french_stem", "german_stem", "russian_stem"} {
		filterTypes[name] = filterDef{attrs: attrKeyword, build: unsupportedFilter}
	}
	for _, name := range []string{"min_hash", "concatenate_graph", "delimited_term_freq", "hyphenation_decompounder", "condition", "predicate_token_filter"} {
		filterTypes[name] = filterDef{build: unsupportedFilter}
	}
}

// ---------------------------------------------------------------------------
// analyzers and normalizers
// ---------------------------------------------------------------------------

// englishStopWords is Lucene's EnglishAnalyzer.ENGLISH_STOP_WORDS_SET.
var englishStopWords = strings.Fields("a an and are as at be but by for if in into is it no not of on or such that the their then there these they this to was will with")

// cjkStopWords is Lucene's CJKAnalyzer default stop set.
var cjkStopWords = strings.Fields("a and are as at be but by for if in into is it no not of on or s such t that the their then there these they this to was will with www")

var bleveStopWordLists = map[string][]byte{
	"_arabic_": blevear.ArabicStopWords, "_armenian_": blevehy.ArmenianStopWords, "_basque_": bleveeu.BasqueStopWords,
	"_bulgarian_": blevebg.BulgarianStopWords, "_catalan_": bleveca.CatalanStopWords, "_czech_": blevecs.CzechStopWords,
	"_danish_": bleveda.DanishStopWords, "_dutch_": blevenl.DutchStopWords, "_finnish_": blevefi.FinnishStopWords,
	"_french_": blevefr.FrenchStopWords, "_galician_": blevegl.GalicianStopWords, "_german_": blevede.GermanStopWords,
	"_greek_": bleveel.GreekStopWords, "_hindi_": blevehi.HindiStopWords, "_hungarian_": blevehu.HungarianStopWords,
	"_indonesian_": bleveid.IndonesianStopWords, "_irish_": blevega.IrishStopWords, "_italian_": bleveit.ItalianStopWords,
	"_norwegian_": bleveno.NorwegianStopWords, "_persian_": blevefa.PersianStopWords, "_portuguese_": blevept.PortugueseStopWords,
	"_romanian_": blevero.RomanianStopWords, "_russian_": bleveru.RussianStopWords, "_sorani_": bleveckb.SoraniStopWords,
	"_spanish_": blevees.SpanishStopWords, "_swedish_": blevesv.SwedishStopWords, "_turkish_": blevetr.TurkishStopWords,
}

// namedStopWords resolves a _lang_ stop word set; known is false for other
// words (which are then literal stop words).
func namedStopWords(name string) (words []string, known bool, err error) {
	switch name {
	case "_english_":
		return englishStopWords, true, nil
	case "_cjk_":
		return cjkStopWords, true, nil
	case "_bengali_", "_brazilian_", "_estonian_", "_latvian_", "_lithuanian_", "_thai_":
		return nil, true, unsupported("the stop word set [%s]", name)
	}
	data, ok := bleveStopWordLists[name]
	if !ok {
		return nil, false, nil
	}
	tm := analysis.NewTokenMap()
	if err := tm.LoadBytes(data); err != nil {
		return nil, true, err
	}
	for w := range tm {
		words = append(words, w)
	}
	return words, true, nil
}

// cjkWidthCharFilter is CJKWidthCharFilter: fullwidth ASCII to basic Latin,
// halfwidth katakana to fullwidth (combining voiced sound marks).
type cjkWidthCharFilter struct{}

func (cjkWidthCharFilter) filter(text string) (string, *offsetMap) {
	in := []rune(text)
	f := blevecjk.NewCJKWidthFilter()
	width := func(s string) string {
		ts := f.Filter(analysis.TokenStream{&analysis.Token{Term: []byte(s)}})
		return string(ts[0].Term)
	}
	m := &offsetMap{}
	var out []rune
	cum := 0
	for i := 0; i < len(in); i++ {
		r := in[i]
		if i+1 < len(in) && (in[i+1] == 0xFF9E || in[i+1] == 0xFF9F) && r >= 0xFF65 && r <= 0xFF9D {
			if c := []rune(width(string(in[i : i+2]))); len(c) == 1 {
				out = append(out, c[0])
				cum++
				m.add(len(out), cum)
				i++
				continue
			}
		}
		if (r >= 0xFF01 && r <= 0xFF5E) || (r >= 0xFF65 && r <= 0xFF9F) {
			out = append(out, []rune(width(string(r)))...)
			continue
		}
		out = append(out, r)
	}
	return string(out), m
}

var unsupportedLanguageAnalyzers = map[string]bool{
	"arabic": true, "armenian": true, "basque": true, "bengali": true, "brazilian": true, "bulgarian": true, "catalan": true,
	"czech": true, "danish": true, "dutch": true, "estonian": true, "finnish": true, "french": true, "galician": true, "german": true,
	"greek": true, "hindi": true, "hungarian": true, "indonesian": true, "irish": true, "italian": true, "latvian": true,
	"lithuanian": true, "norwegian": true, "persian": true, "portuguese": true, "romanian": true, "russian": true, "sorani": true,
	"spanish": true, "swedish": true, "turkish": true, "thai": true,
}

func namedFilter(name string, tf anTokenFilter, attrs int) namedTokenFilter {
	return namedTokenFilter{name: name, tf: tf, attrs: attrs}
}

// builtinAnalyzer builds a prebuilt analyzer or an analyzer of a built-in
// type configured with settings s. known is false for unknown types.
func (ctx *anContext) builtinAnalyzer(typ, name string, s M) (*anAnalyzer, bool, error) {
	b := &compBuild{ctx: ctx, name: name, kind: "analyzer", s: s}
	a := &anAnalyzer{name: name, offsetGap: 1}
	stop := func(def []string) (namedTokenFilter, error) {
		words, _, err := b.words("stopwords", def, false, true)
		if err != nil {
			return namedTokenFilter{}, err
		}
		if words == nil {
			words = newWordSet(nil, false)
		}
		return namedFilter("stop", stopTokenFilter(words, true), 0), nil
	}
	lower := namedFilter("lowercase", mapTerms(lowercaseFilter), 0)
	standard := func() error {
		n, err := scannerMaxTokenLength(b)
		a.tokenizer = namedTokenizer{name: "standard", tok: standardTokenizer{maxLen: n}}
		return err
	}
	var err error
	switch typ {
	case "standard", "default", "chinese":
		if err = standard(); err != nil {
			return nil, true, err
		}
		sf, err := stop(nil)
		a.filters = []namedTokenFilter{lower, sf}
		return a, true, err
	case "simple":
		a.tokenizer = namedTokenizer{name: "lowercase", tok: charTokenizer{isTokenChar: unicode.IsLetter, normalize: javaToLower, maxLen: 255}}
	case "whitespace":
		a.tokenizer = namedTokenizer{name: "whitespace", tok: charTokenizer{isTokenChar: func(r rune) bool { return !javaIsWhitespace(r) }, maxLen: 255}}
	case "keyword":
		a.tokenizer = namedTokenizer{name: "keyword", tok: keywordTokenizer{}}
	case "stop":
		a.tokenizer = namedTokenizer{name: "lowercase", tok: charTokenizer{isTokenChar: unicode.IsLetter, normalize: javaToLower, maxLen: 255}}
		sf, err := stop(englishStopWords)
		a.filters = []namedTokenFilter{sf}
		return a, true, err
	case "pattern":
		re, err := compileJavaRegex(b.str("pattern", `\W+`), b.str("flags", ""))
		if err != nil {
			return nil, true, err
		}
		a.tokenizer = namedTokenizer{name: "pattern", tok: patternTokenizer{re: re, group: -1}}
		lc, err := b.bool("lowercase", true)
		if err != nil {
			return nil, true, err
		}
		if lc {
			a.filters = append(a.filters, lower)
		}
		sf, err := stop(nil)
		a.filters = append(a.filters, sf)
		return a, true, err
	case "fingerprint":
		if err = standard(); err != nil {
			return nil, true, err
		}
		sep := b.str("separator", " ")
		if utf16Len(sep) != 1 {
			return nil, true, errAnalysis("Setting [separator] must be a single, non-null character. [%s] was provided.", sep)
		}
		n, err := b.int("max_output_size", 255)
		if err != nil {
			return nil, true, err
		}
		sf, err := stop(nil)
		a.filters = []namedTokenFilter{lower, namedFilter("asciifolding", asciiFoldingChainFilter(false), 0), sf, namedFilter("fingerprint", fingerprintFilter(sep, n), 0)}
		return a, true, err
	case "classic":
		n, err := scannerMaxTokenLength(b)
		if err != nil {
			return nil, true, err
		}
		a.tokenizer = namedTokenizer{name: "classic", tok: classicTokenizer{maxLen: n}}
		sf, err := stop(englishStopWords)
		a.filters = []namedTokenFilter{namedFilter("classic", mapTerms(classicFilter), 0), lower, sf}
		return a, true, err
	case "english":
		if err = standard(); err != nil {
			return nil, true, err
		}
		sf, err := stop(englishStopWords)
		if err != nil {
			return nil, true, err
		}
		a.filters = []namedTokenFilter{namedFilter("possessive", mapTerms(stemFilter(englishPossessive)), 0), lower, sf}
		if excl, ok := b.list("stem_exclusion"); ok {
			a.filters = append(a.filters, namedFilter("keyword_marker", mapTerms(keywordMarkerFilter(newWordSet(excl, false), nil)), attrKeyword))
		}
		a.filters = append(a.filters, namedFilter("porter_stem", mapTerms(stemFilter(porterStem)), attrKeyword))
		return a, true, nil
	case "cjk":
		if err = standard(); err != nil {
			return nil, true, err
		}
		a.charFilters = []namedCharFilter{{name: "cjk_width", cf: cjkWidthCharFilter{}}}
		sf, err := stop(cjkStopWords)
		a.filters = []namedTokenFilter{lower, namedFilter("cjk_bigram", cjkBigramFilter{han: true, hiragana: true, katakana: true, hangul: true}, 0), sf}
		return a, true, err
	case "snowball":
		lang := b.str("language", "English")
		stem, ok, err := snowballForLanguage(lang)
		if err != nil {
			return nil, true, err
		}
		if !ok {
			return nil, true, unsupported("the snowball analyzer language [%s]", lang)
		}
		if err = standard(); err != nil {
			return nil, true, err
		}
		if lang == "English" || lang == "Porter" {
			a.filters = append(a.filters, namedFilter("possessive", mapTerms(stemFilter(englishPossessive)), 0))
		}
		if lang == "Turkish" {
			a.filters = append(a.filters, namedFilter("lowercase", mapTerms(turkishLowercaseFilter), 0))
		} else {
			a.filters = append(a.filters, lower)
		}
		var def []string
		switch lang {
		case "English":
			def = englishStopWords
		case "German", "Dutch":
			def, _, _ = namedStopWords("_" + strings.ToLower(lang) + "_")
		}
		sf, err := stop(def)
		a.filters = append(a.filters, sf, namedFilter("snowball", mapTerms(stemFilter(stem)), attrKeyword))
		return a, true, err
	case "kuromoji", "japanese":
		if japaneseAnalyzerType == "" || (typ == "japanese" && name == "japanese" && !ctx.hasIndex) {
			return nil, false, nil
		}
		a.tokenizer = namedTokenizer{name: "kuromoji_tokenizer", tok: newKuromojiTokenizer(s, []any{
			M{"type": "kuromoji_baseform"}, M{"type": "kuromoji_part_of_speech"}, M{"type": "cjk_width"},
			M{"type": "ja_stop"}, M{"type": "kuromoji_stemmer"}, M{"type": "lowercase"},
		})}
	default:
		if ok, err := ctx.languageAnalyzer(typ, a, b); ok {
			return a, true, err
		}
		if unsupportedLanguageAnalyzers[typ] {
			return nil, true, unsupported("the [%s] analyzer", typ)
		}
		return nil, false, nil
	}
	return a, true, err
}

// analyzerByName resolves the analyzer of an _analyze request.
func (ctx *anContext) analyzerByName(name string) (*anAnalyzer, error) {
	if ctx.hasIndex {
		return ctx.indexAnalyzer(name)
	}
	a, known, err := ctx.builtinAnalyzer(name, name, M{})
	if !known {
		return nil, errAnalysis("failed to find global analyzer [%s]", name)
	}
	return a, err
}

// indexAnalyzer resolves an analyzer of the index (IndexAnalyzers.get).
func (ctx *anContext) indexAnalyzer(name string) (*anAnalyzer, error) {
	if def, ok := getMap(ctx.analysis, "analyzer")[name].(M); ok {
		return ctx.analyzerFromDef(name, def)
	}
	a, known, err := ctx.builtinAnalyzer(name, name, M{})
	if !known {
		return nil, errAnalysis("failed to find analyzer [%s]", name)
	}
	if a != nil {
		a.posGap = 100
	}
	return a, err
}

// analyzerFromDef builds an analyzer defined in the index settings.
func (ctx *anContext) analyzerFromDef(name string, def M) (*anAnalyzer, error) {
	typ := getString(def, "type")
	if typ == "" {
		if _, ok := def["tokenizer"]; !ok {
			return nil, errAnalysis("analyzer [%s] must specify either an analyzer type, or a tokenizer", name)
		}
		typ = "custom"
	}
	if typ != "custom" {
		a, known, err := ctx.builtinAnalyzer(typ, name, withoutType(def))
		if !known {
			return nil, errAnalysis("Unknown analyzer type [%s] for [%s]", typ, name)
		}
		if a != nil {
			a.posGap = 100
		}
		return a, err
	}
	b := &compBuild{ctx: ctx, name: name, kind: "analyzer", s: def}
	a := &anAnalyzer{name: name, custom: true, offsetGap: 1}
	var err error
	if a.posGap, err = b.int("position_increment_gap", 100); err != nil {
		return nil, err
	}
	if og, err := b.int("offset_gap", -1); err != nil {
		return nil, err
	} else if og >= 0 {
		a.offsetGap = og
	}
	tokName := b.str("tokenizer", "")
	if tokName == "" {
		return nil, errAnalysis("Custom Analyzer [%s] must be configured with a tokenizer", name)
	}
	notFound := func(kind, ref string, err error) error {
		var e *Error
		if errors.As(err, &e) && strings.HasPrefix(e.Reason, "failed to find "+kind+" under") {
			return errAnalysis("Custom Analyzer [%s] failed to find %s under name [%s]", name, kind, ref)
		}
		return err
	}
	cfNames, _ := b.list("char_filter")
	for _, cfName := range cfNames {
		cf, err := ctx.charFilter(cfName)
		if err != nil {
			return nil, notFound("char_filter", cfName, err)
		}
		a.charFilters = append(a.charFilters, cf)
	}
	tok, err := ctx.tokenizer(tokName)
	if err != nil {
		return nil, notFound("tokenizer", tokName, err)
	}
	a.tokenizer = tok
	fNames, _ := b.list("filter")
	for _, fName := range fNames {
		chain := &anAnalyzer{charFilters: a.charFilters, tokenizer: a.tokenizer, filters: append([]namedTokenFilter(nil), a.filters...)}
		tf, _, err := ctx.tokenFilter(fName, chain)
		if err != nil {
			return nil, notFound("filter", fName, err)
		}
		a.filters = append(a.filters, tf)
	}
	foldKuromoji(a)
	return a, nil
}

// normalizerByName resolves a normalizer of the index.
func (ctx *anContext) normalizerByName(name string) (*anAnalyzer, error) {
	if def, ok := getMap(ctx.analysis, "normalizer")[name].(M); ok {
		return ctx.normalizerFromDef(name, def)
	}
	if name == "lowercase" {
		return &anAnalyzer{name: "lowercase", custom: true, offsetGap: 1, tokenizer: namedTokenizer{name: "keyword", tok: keywordTokenizer{}},
			filters: []namedTokenFilter{namedFilter("lowercase", mapTerms(lowercaseFilter), 0)}}, nil
	}
	return nil, errAnalysis("failed to find normalizer under [%s]", name)
}

func (ctx *anContext) normalizerFromDef(name string, def M) (*anAnalyzer, error) {
	if typ := getString(def, "type"); typ != "" && typ != "custom" {
		return nil, errAnalysis("Unknown normalizer type [%s] for [%s]", typ, name)
	}
	b := &compBuild{ctx: ctx, name: name, kind: "normalizer", s: def}
	a := &anAnalyzer{name: name, custom: true, offsetGap: 1, tokenizer: namedTokenizer{name: "keyword", tok: keywordTokenizer{}}}
	cfNames, _ := b.list("char_filter")
	for _, cfName := range cfNames {
		cf, err := ctx.charFilter(cfName)
		if err != nil {
			var e *Error
			if errors.As(err, &e) && strings.HasPrefix(e.Reason, "failed to find char_filter under") {
				return nil, errAnalysis("Custom Analyzer [%s] failed to find char_filter under name [%s]", name, cfName)
			}
			return nil, err
		}
		a.charFilters = append(a.charFilters, cf)
	}
	fNames, _ := b.list("filter")
	for _, fName := range fNames {
		tf, normalizing, err := ctx.tokenFilter(fName, nil)
		if err != nil {
			var e *Error
			if errors.As(err, &e) && strings.HasPrefix(e.Reason, "failed to find filter under") {
				return nil, errAnalysis("Custom Analyzer [%s] failed to find filter under name [%s]", name, fName)
			}
			return nil, err
		}
		if !normalizing {
			return nil, errAnalysis("Custom normalizer [%s] may not use filter [%s]", name, fName)
		}
		a.filters = append(a.filters, tf)
	}
	return a, nil
}

var keywordFieldAnalyzer = &anAnalyzer{name: "_keyword", offsetGap: 1, tokenizer: namedTokenizer{name: "keyword", tok: keywordTokenizer{}}}

// fieldAnalyzer is the index analyzer of a mapped field (MappedFieldType.
// indexAnalyzer), as used by field-based _analyze requests.
func (ctx *anContext) fieldAnalyzer(ix *Index, field string) (*anAnalyzer, error) {
	f, _, ok := ix.Mapping.resolve(field)
	if !ok || f.Type == TypeObject || f.Type == TypeNested {
		return ctx.indexAnalyzer("default")
	}
	switch f.Type {
	case TypeText, TypeSearchAsYouType, "match_only_text":
		name := f.Analyzer
		if name == "" {
			name = "default"
		}
		a, err := ctx.indexAnalyzer(name)
		if err != nil {
			return nil, err
		}
		cp := *a
		cp.posGap = 100
		if v, ok := f.Extra["position_increment_gap"]; ok {
			cp.posGap = int(toInt64(v, 100))
		}
		return &cp, nil
	case TypeKeyword, TypeWildcard:
		if f.Normalizer != "" {
			return ctx.normalizerByName(f.Normalizer)
		}
		return keywordFieldAnalyzer, nil
	case TypeFlatObject:
		return nil, &Error{Status: 500, Type: "null_pointer_exception", Reason: `Cannot invoke "org.apache.lucene.analysis.Analyzer.tokenStream(String, String)" because "analyzer" is null`}
	}
	return nil, errAnalysis("Can't process field [%s], Analysis requests are only supported on tokenized fields", field)
}

// ---------------------------------------------------------------------------
// Japanese (osmem/ja): kuromoji runs as one bleve analyzer
// ---------------------------------------------------------------------------

var kuromojiFilterTypes = []string{"kuromoji_baseform", "kuromoji_part_of_speech", "ja_stop", "kuromoji_stemmer", "kuromoji_readingform", "kuromoji_number"}

// kuromojiStep is a kuromoji token filter; it only takes effect folded into a
// kuromoji tokenizer.
type kuromojiStep struct{ spec M }

func (kuromojiStep) apply(in anStream) anStream { return in }

type kuromojiTokenizer struct {
	settings M
	steps    []any
	once     sync.Once
	an       analysis.Analyzer
}

func newKuromojiTokenizer(settings M, steps []any) *kuromojiTokenizer {
	return &kuromojiTokenizer{settings: settings, steps: steps}
}

func (k *kuromojiTokenizer) tokenize(text string) anStream {
	k.once.Do(func() {
		cfg := M{"type": japaneseAnalyzerType, "filters": k.steps}
		for _, key := range []string{"mode", "discard_punctuation", "user_dictionary", "user_dictionary_rules", "discard_compound_token"} {
			if v, ok := k.settings[key]; ok {
				cfg[key] = v
			}
		}
		k.an, _ = registry.NewCache().DefineAnalyzer("osmem_kuromoji", cfg)
	})
	u := utf16Positions(text)
	out := anStream{finalOffset: u(len(text))}
	if k.an == nil {
		return out
	}
	prev := 0
	for _, bt := range k.an.Analyze([]byte(text)) {
		inc := bt.Position - prev
		if inc < 0 {
			inc = 0
		}
		prev = bt.Position
		start, end := bt.Start, bt.End
		if start > len(text) {
			start = len(text)
		}
		if end > len(text) {
			end = len(text)
		}
		out.tokens = append(out.tokens, anToken{term: string(bt.Term), start: u(start), end: u(end), posInc: inc, posLen: 1, typ: "word"})
	}
	return out
}

// foldKuromoji moves kuromoji filters that directly follow a kuromoji
// tokenizer into it.
func foldKuromoji(a *anAnalyzer) {
	kt, ok := a.tokenizer.tok.(*kuromojiTokenizer)
	if !ok {
		return
	}
	steps := append([]any(nil), kt.steps...)
	var rest []namedTokenFilter
	for _, f := range a.filters {
		if step, ok := f.tf.(kuromojiStep); ok && len(rest) == 0 {
			steps = append(steps, step.spec)
			continue
		}
		rest = append(rest, f)
	}
	a.tokenizer.tok = newKuromojiTokenizer(kt.settings, steps)
	a.filters = rest
}

func init() {
	tokenizerTypes["kuromoji_tokenizer"] = func(b *compBuild) (anTokenizer, error) {
		// user_dictionary names a file on the server; like the other *_path
		// settings osmem never reads files named by a request. Inline rules
		// (user_dictionary_rules) are supported instead.
		if _, has := b.raw("user_dictionary"); has {
			return nil, unsupported("[user_dictionary] of tokenizer [%s]; use user_dictionary_rules", b.name)
		}
		return newKuromojiTokenizer(b.s, nil), nil
	}
	for _, name := range kuromojiFilterTypes {
		typ := name
		filterTypes[typ] = filterDef{build: func(b *compBuild) (anTokenFilter, error) {
			spec := M{"type": typ}
			for k, v := range b.s {
				spec[k] = v
			}
			return kuromojiStep{spec: spec}, nil
		}}
	}
}

func kuromojiComponent(kind, typ string) bool {
	if kind == "tokenizer" {
		return typ == "kuromoji_tokenizer"
	}
	if kind == "filter" {
		for _, n := range kuromojiFilterTypes {
			if n == typ {
				return true
			}
		}
	}
	return false
}

// languageAnalyzer builds the Lucene language analyzers osmem emulates
// (stop words from bleve's copies of the Lucene/Snowball lists).
func (ctx *anContext) languageAnalyzer(typ string, a *anAnalyzer, b *compBuild) (bool, error) {
	n, err := scannerMaxTokenLength(b)
	if err != nil {
		return true, err
	}
	lower := namedFilter("lowercase", mapTerms(lowercaseFilter), 0)
	stop := func(named string) (namedTokenFilter, error) {
		def, _, err := namedStopWords(named)
		if err != nil {
			return namedTokenFilter{}, err
		}
		words, _, err := b.words("stopwords", def, false, true)
		if err != nil {
			return namedTokenFilter{}, err
		}
		return namedFilter("stop", stopTokenFilter(words, true), 0), nil
	}
	exclusion := func() []namedTokenFilter {
		if excl, ok := b.list("stem_exclusion"); ok && len(excl) > 0 {
			return []namedTokenFilter{namedFilter("keyword_marker", mapTerms(keywordMarkerFilter(newWordSet(excl, false), nil)), attrKeyword)}
		}
		return nil
	}
	stem := func(f func(string) string) namedTokenFilter {
		return namedFilter("stemmer", mapTerms(stemFilter(f)), attrKeyword)
	}
	bleveTerm := func(name string, f analysis.TokenFilter) namedTokenFilter {
		return namedFilter(name, mapTerms(func(t *anToken) { t.term = bleveTermFilter(f)(t.term) }), 0)
	}
	articles := func(data []byte) *wordSet {
		tm := analysis.NewTokenMap()
		_ = tm.LoadBytes(data)
		set := newWordSet(nil, true)
		for w := range tm {
			set.add(w)
		}
		return set
	}
	decimal := namedFilter("decimal_digit", mapTerms(decimalDigitFilter), 0)
	a.tokenizer = namedTokenizer{name: "standard", tok: standardTokenizer{maxLen: n}}
	var chain []namedTokenFilter
	add := func(fs ...namedTokenFilter) { chain = append(chain, fs...) }
	sf, err := stop("_" + typ + "_")
	if err != nil {
		return true, err
	}
	switch typ {
	case "danish", "finnish", "hungarian", "norwegian", "swedish", "russian", "romanian":
		add(lower, sf)
		add(exclusion()...)
		add(stem(snowballStem(snowballStemmers[typ])))
	case "dutch":
		add(lower, sf)
		add(exclusion()...)
		add(namedFilter("stemmer_override", mapTerms(stemmerOverrideFilter(map[string]string{"fiets": "fiets", "bromfiets": "bromfiets", "ei": "eier", "kind": "kinder"})), attrKeyword))
		add(stem(snowballStem(snowballStemmers["dutch"])))
	case "french":
		add(namedFilter("elision", mapTerms(elisionFilter(newWordSet([]string{"l", "m", "t", "qu", "n", "s", "j", "d", "c", "jusqu", "quoiqu", "lorsqu", "puisqu"}, true))), 0), lower, sf)
		add(exclusion()...)
		add(stem(bleveTermFilter(blevefr.NewFrenchLightStemmerFilter())))
	case "german":
		add(lower, sf)
		add(exclusion()...)
		add(bleveTerm("german_normalization", blevede.NewGermanNormalizeFilter()), stem(bleveTermFilter(blevede.NewGermanLightStemmerFilter())))
	case "spanish":
		add(lower, sf)
		add(exclusion()...)
		add(stem(spanishLightStem))
	case "italian":
		add(namedFilter("elision", mapTerms(elisionFilter(articles(bleveit.ItalianArticles))), 0), lower, sf)
		add(exclusion()...)
		add(stem(bleveTermFilter(bleveit.NewItalianLightStemmerFilterFilter())))
	case "portuguese":
		add(lower, sf)
		add(exclusion()...)
		add(stem(bleveTermFilter(blevept.NewPortugueseLightStemmerFilter())))
	case "arabic":
		add(lower, decimal, sf, bleveTerm("arabic_normalization", blevear.NewArabicNormalizeFilter()))
		add(exclusion()...)
		add(stem(bleveTermFilter(blevear.NewArabicStemmerFilter())))
	case "turkish":
		add(namedFilter("apostrophe", mapTerms(apostropheFilter), 0), namedFilter("lowercase", mapTerms(turkishLowercaseFilter), 0), sf)
		add(exclusion()...)
		add(stem(snowballStem(snowballStemmers["turkish"])))
	case "irish":
		add(namedFilter("stop", stopTokenFilter(newWordSet([]string{"h", "n", "t"}, true), true), 0),
			namedFilter("elision", mapTerms(elisionFilter(newWordSet([]string{"d", "m", "b"}, true))), 0),
			namedFilter("lowercase", mapTerms(irishLowercaseFilter), 0), sf)
		add(exclusion()...)
		add(stem(snowballStem(snowballStemmers["irish"])))
	case "hindi":
		add(lower, decimal)
		add(exclusion()...)
		add(bleveTerm("indic_normalization", blevein.NewIndicNormalizeFilter()), bleveTerm("hindi_normalization", blevehi.NewHindiNormalizeFilter()), sf,
			stem(bleveTermFilter(blevehi.NewHindiStemmerFilter())))
	case "sorani":
		add(bleveTerm("sorani_normalization", bleveckb.NewSoraniNormalizeFilter()), lower, decimal, sf)
		add(exclusion()...)
		add(stem(bleveTermFilter(bleveckb.NewSoraniStemmerFilter())))
	default:
		a.tokenizer = namedTokenizer{}
		return false, nil
	}
	a.filters = chain
	return true, nil
}

var spanishAccents = strings.NewReplacer("\u00e0", "a", "\u00e1", "a", "\u00e2", "a", "\u00e4", "a", "\u00f2", "o", "\u00f3", "o", "\u00f4", "o", "\u00f6", "o",
	"\u00e8", "e", "\u00e9", "e", "\u00ea", "e", "\u00eb", "e", "\u00f9", "u", "\u00fa", "u", "\u00fb", "u", "\u00fc", "u", "\u00ec", "i", "\u00ed", "i", "\u00ee", "i", "\u00ef", "i")

// spanishLightStem is SpanishLightStemmer: vowel accents are folded before
// bleve's port of the stemming rules runs.
func spanishLightStem(s string) string {
	if utf16Len(s) < 5 {
		return s
	}
	return spanishLightStemmer(spanishAccents.Replace(s))
}

var spanishLightStemmer = bleveTermFilter(blevees.NewSpanishLightStemmerFilter())

// ---------------------------------------------------------------------------
// index analysis settings validation
// ---------------------------------------------------------------------------

// createTimeError is an error Lucene raises only when a component is used
// (TokenStream creation), not when the index is created.
type createTimeError struct{ err *Error }

func (e createTimeError) Error() string { return e.err.Error() }

func (e createTimeError) Unwrap() error { return e.err }

func errCreateTime(format string, args ...any) error {
	return createTimeError{errAnalysis(format, args...)}
}

// isSoftAnalysisError reports errors that do not fail index creation.
func isSoftAnalysisError(err error) bool {
	var u *unsupportedComponent
	var c createTimeError
	return errors.As(err, &u) || errors.As(err, &c)
}

// validateAnalysis raises the errors OpenSearch reports while building the
// analysis components of a new index (AnalysisRegistry.build): unknown or
// missing component types, component construction errors, custom analyzers
// referring to missing components ("Failed to build analyzers") and invalid
// normalizers. Components osmem cannot emulate only produce warnings.
func validateAnalysis(settings M, warn func(string)) error {
	an := analysisSettings(settings)
	if an == nil {
		return nil
	}
	ctx := newAnContext(settings, true)
	soft := func(err error) error {
		if err == nil {
			return nil
		}
		var u *unsupportedComponent
		if errors.As(err, &u) {
			if warn != nil {
				warn(u.Error())
			}
			return nil
		}
		if isSoftAnalysisError(err) {
			return nil
		}
		return err
	}
	for _, kind := range []string{"char_filter", "tokenizer", "filter"} {
		defs := getMap(an, kind)
		for _, name := range sortedMapKeys(defs) {
			def, _ := defs[name].(M)
			typ := getString(def, "type")
			if typ == "" {
				return errAnalysis("%s [%s] must specify either an analyzer type, or a tokenizer", kind, name)
			}
			if !knownComponentType(kind, typ) {
				return errAnalysis("Unknown %s type [%s] for [%s]", kind, typ, name)
			}
			var err error
			switch kind {
			case "char_filter":
				_, err = ctx.charFilter(name)
			case "tokenizer":
				_, err = ctx.tokenizer(name)
			default:
				switch typ {
				case "synonym", "synonym_graph", "multiplexer", "condition":
					continue // built with the analyzer chain
				}
				_, _, err = ctx.tokenFilter(name, nil)
			}
			if err := soft(err); err != nil {
				return err
			}
		}
	}
	analyzers := getMap(an, "analyzer")
	var custom []string
	for _, name := range sortedMapKeys(analyzers) {
		def, _ := analyzers[name].(M)
		typ := getString(def, "type")
		if typ == "" {
			if _, ok := def["tokenizer"]; !ok {
				return errAnalysis("analyzer [%s] must specify either an analyzer type, or a tokenizer", name)
			}
			typ = "custom"
		}
		if typ == "custom" {
			custom = append(custom, name)
			continue
		}
		_, known, err := ctx.builtinAnalyzer(typ, name, withoutType(def))
		if !known {
			return errAnalysis("Unknown analyzer type [%s] for [%s]", typ, name)
		}
		if err := soft(err); err != nil {
			return err
		}
	}
	normalizers := getMap(an, "normalizer")
	for _, name := range sortedMapKeys(normalizers) {
		def, _ := normalizers[name].(M)
		if typ := getString(def, "type"); typ != "" && typ != "custom" {
			return errAnalysis("Unknown normalizer type [%s] for [%s]", typ, name)
		}
	}
	var failed []string
	for _, name := range custom {
		def, _ := analyzers[name].(M)
		if _, err := ctx.analyzerFromDef(name, def); soft(err) != nil {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return errAnalysis("Failed to build analyzers: [%s]", strings.Join(failed, ", "))
	}
	for _, name := range sortedMapKeys(normalizers) {
		def, _ := normalizers[name].(M)
		if _, err := ctx.normalizerFromDef(name, def); soft(err) != nil {
			return err
		}
	}
	return nil
}
