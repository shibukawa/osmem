// Package ja adds Japanese morphological analysis to osmem using
// kagome (https://github.com/ikawaha/kagome), a pure Go port of MeCab with
// the IPA dictionary. Importing the package for its side effects
//
//	import _ "github.com/shibukawa/osmem/ja"
//
// makes OpenSearch's kuromoji analyzer, the kuromoji_tokenizer (modes
// normal/search/extended, discard_punctuation, user_dictionary_rules) and
// the token filters kuromoji_baseform, kuromoji_part_of_speech (stoptags),
// cjk_width, ja_stop (stopwords), kuromoji_stemmer (minimum_length),
// kuromoji_readingform (katakana reading) and lowercase available in index
// settings, instead of the CJK bigram fallback.
package ja

import (
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/registry"
	"github.com/ikawaha/kagome-dict/dict"
	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
	"golang.org/x/text/width"

	"github.com/shibukawa/osmem/internal/engine"
)

// AnalyzerType is the bleve analyzer type name registered by this package.
const AnalyzerType = "osmem_ja"

var (
	dictMu   sync.Mutex
	baseDict *dict.Dict
)

// SetDict replaces the system dictionary (default: kagome-dict/ipa). Call
// it before creating clusters, for example with ipa.DictShrink() or a
// UniDic dictionary.
func SetDict(d *dict.Dict) {
	dictMu.Lock()
	defer dictMu.Unlock()
	baseDict = d
}

func systemDict() *dict.Dict {
	dictMu.Lock()
	defer dictMu.Unlock()
	if baseDict == nil {
		baseDict = ipa.Dict()
	}
	return baseDict
}

func init() {
	if err := registry.RegisterAnalyzer(AnalyzerType, newAnalyzer); err != nil {
		panic(err)
	}
	engine.RegisterJapaneseAnalyzer(AnalyzerType)
}

// SetEnabled turns the Japanese analysis on or off for clusters created
// afterwards. Disabled, kuromoji falls back to the CJK bigram analyzer as
// on an OpenSearch cluster without the analysis-kuromoji plugin.
func SetEnabled(enabled bool) {
	if enabled {
		engine.RegisterJapaneseAnalyzer(AnalyzerType)
	} else {
		engine.RegisterJapaneseAnalyzer("")
	}
}

type step struct {
	kind      string
	stoptags  map[string]bool
	stopwords map[string]bool
	minLength int
	useRomaji bool
}

type analyzer struct {
	tok          *tokenizer.Tokenizer
	mode         tokenizer.TokenizeMode
	discardPunct bool
	steps        []step
	charFilters  []analysis.CharFilter
	tokenFilters []analysis.TokenFilter
}

func newAnalyzer(config map[string]interface{}, cache *registry.Cache) (analysis.Analyzer, error) {
	a := &analyzer{mode: tokenizer.Search, discardPunct: true}
	switch strings.ToLower(str(config["mode"])) {
	case "", "search":
		a.mode = tokenizer.Search
	case "normal":
		a.mode = tokenizer.Normal
	case "extended":
		a.mode = tokenizer.Extended
	default:
		return nil, fmt.Errorf("kuromoji_tokenizer: unknown mode [%v]", config["mode"])
	}
	if v, ok := config["discard_punctuation"]; ok {
		a.discardPunct = boolean(v, true)
	}
	opts := []tokenizer.Option{tokenizer.OmitBosEos()}
	var rules []string
	switch r := config["user_dictionary_rules"].(type) {
	case []interface{}:
		for _, e := range r {
			rules = append(rules, str(e))
		}
	case []string:
		rules = r
	case string:
		rules = strings.Split(r, "\n")
	}
	if len(rules) > 0 {
		records, err := dict.NewUserDicRecords(strings.NewReader(strings.Join(rules, "\n") + "\n"))
		if err != nil {
			return nil, fmt.Errorf("kuromoji_tokenizer: user_dictionary_rules: %w", err)
		}
		ud, err := records.NewUserDict()
		if err != nil {
			return nil, fmt.Errorf("kuromoji_tokenizer: user_dictionary_rules: %w", err)
		}
		opts = append(opts, tokenizer.UserDict(ud))
	} else if str(config["user_dictionary"]) != "" {
		// a file path from index settings is never opened: the engine rejects
		// the setting before it gets here, and this guards direct callers.
		return nil, fmt.Errorf("kuromoji_tokenizer: user_dictionary is not supported; use user_dictionary_rules")
	}
	t, err := tokenizer.New(systemDict(), opts...)
	if err != nil {
		return nil, err
	}
	a.tok = t
	if fs, ok := config["filters"].([]interface{}); ok {
		for _, raw := range fs {
			spec, _ := raw.(map[string]interface{})
			s, err := parseStep(spec)
			if err != nil {
				return nil, err
			}
			a.steps = append(a.steps, s)
		}
	}
	if cfs, ok := config["char_filters"].([]interface{}); ok {
		for _, n := range cfs {
			cf, err := cache.CharFilterNamed(str(n))
			if err != nil {
				return nil, err
			}
			a.charFilters = append(a.charFilters, cf)
		}
	}
	if tfs, ok := config["token_filters"].([]interface{}); ok {
		for _, n := range tfs {
			tf, err := cache.TokenFilterNamed(str(n))
			if err != nil {
				return nil, err
			}
			a.tokenFilters = append(a.tokenFilters, tf)
		}
	}
	return a, nil
}

func parseStep(spec map[string]interface{}) (step, error) {
	s := step{kind: str(spec["type"])}
	switch s.kind {
	case "kuromoji_part_of_speech":
		s.stoptags = map[string]bool{}
		if tags, ok := spec["stoptags"].([]interface{}); ok {
			for _, t := range tags {
				s.stoptags[str(t)] = true
			}
		} else {
			for _, t := range defaultStopTags {
				s.stoptags[t] = true
			}
		}
	case "ja_stop":
		s.stopwords = map[string]bool{}
		words, ok := spec["stopwords"]
		if !ok {
			words = "_japanese_"
		}
		var list []interface{}
		switch w := words.(type) {
		case string:
			list = []interface{}{w}
		case []interface{}:
			list = w
		}
		for _, w := range list {
			ws := str(w)
			if ws == "_japanese_" {
				for _, sw := range defaultStopWords {
					s.stopwords[sw] = true
				}
				continue
			}
			if ws != "_none_" {
				s.stopwords[ws] = true
			}
		}
	case "kuromoji_stemmer":
		s.minLength = 4
		if v, ok := number(spec["minimum_length"]); ok {
			s.minLength = v
		}
	case "kuromoji_readingform":
		s.useRomaji = boolean(spec["use_romaji"], false)
	case "kuromoji_baseform", "cjk_width", "lowercase", "kuromoji_number":
	default:
		return s, fmt.Errorf("unknown kuromoji filter [%s]", s.kind)
	}
	return s, nil
}

type token struct {
	term    string
	pos     string // "名詞-固有名詞-地域-一般"
	base    string
	reading string
	start   int
	end     int
}

// Analyze implements analysis.Analyzer.
func (a *analyzer) Analyze(input []byte) analysis.TokenStream {
	for _, cf := range a.charFilters {
		input = cf.Filter(input)
	}
	text := string(input)
	var toks []token
	for _, kt := range a.tok.Analyze(text, a.mode) {
		if kt.Class == tokenizer.DUMMY {
			continue
		}
		var parts []string
		for _, p := range kt.POS() {
			if p != "*" && p != "" {
				parts = append(parts, p)
			}
		}
		t := token{term: kt.Surface, pos: strings.Join(parts, "-"), start: kt.Position, end: kt.Position + len(kt.Surface)}
		if b, ok := kt.BaseForm(); ok && b != "*" {
			t.base = b
		}
		if r, ok := kt.Reading(); ok && r != "*" {
			t.reading = r
		}
		if a.discardPunct && (strings.HasPrefix(t.pos, "記号") || isPunctuation(t.term)) {
			continue
		}
		toks = append(toks, t)
	}
	position := 0
	out := make(analysis.TokenStream, 0, len(toks))
	for _, t := range toks {
		position++
		keep := true
		for _, s := range a.steps {
			switch s.kind {
			case "kuromoji_baseform":
				if t.base != "" {
					t.term = t.base
				}
			case "kuromoji_part_of_speech":
				if matchesStopTag(s.stoptags, t.pos) {
					keep = false
				}
			case "cjk_width":
				t.term = width.Fold.String(t.term)
			case "ja_stop":
				if s.stopwords[t.term] {
					keep = false
				}
			case "kuromoji_stemmer":
				t.term = stemKatakana(t.term, s.minLength)
			case "kuromoji_readingform":
				if t.reading != "" {
					t.term = t.reading
				}
				if s.useRomaji {
					t.term = toRomaji(t.term)
				}
			case "lowercase":
				t.term = strings.ToLower(t.term)
			}
			if !keep {
				break
			}
		}
		if !keep || t.term == "" {
			continue
		}
		out = append(out, &analysis.Token{Term: []byte(t.term), Start: t.start, End: t.end, Position: position, Type: analysis.Ideographic})
	}
	for _, tf := range a.tokenFilters {
		out = tf.Filter(out)
	}
	return out
}

func matchesStopTag(tags map[string]bool, pos string) bool {
	if tags[pos] {
		return true
	}
	// a stop tag matches the whole sub-tree of the part of speech
	for i := len(pos) - 1; i > 0; i-- {
		if pos[i] == '-' && tags[pos[:i]] {
			return true
		}
	}
	return false
}

func isPunctuation(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// stemKatakana removes a trailing prolonged sound mark from katakana words
// longer than minLength (kuromoji_stemmer).
func stemKatakana(s string, minLength int) string {
	rs := []rune(s)
	if len(rs) < minLength || rs[len(rs)-1] != 'ー' {
		return s
	}
	for _, r := range rs {
		if !(unicode.In(r, unicode.Katakana) || r == 'ー' || r == '・') {
			return s
		}
	}
	return string(rs[:len(rs)-1])
}

func str(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func boolean(v interface{}, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch t {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return def
}

func number(v interface{}) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		var n int
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n, true
		}
	case interface{ Int64() (int64, error) }:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}
