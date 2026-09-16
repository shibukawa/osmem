package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/document"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
)

// Value parsing of the field types whose mappers read the JSON structure of
// their values (knn_vector, completion, flat_object, rank_feature,
// rank_features, join), with the messages and locations of OpenSearch.

// TypeIgnoredMeta is the type of the _ignored metadata field.
const TypeIgnoredMeta = "_ignored"

// raw JSON tree ------------------------------------------------------------------

// rawNode is a value of the stored source with its byte offsets: start is
// the first byte, tokEnd the end of its first token (after the opening
// bracket of containers) and end the end of the whole value.
type rawNode struct {
	data   []byte
	kind   byte // '{', '[', '"' or the first byte of a literal
	start  int
	tokEnd int
	end    int
	keys   []string   // object keys in document order
	vals   []*rawNode // object values or array elements
	parent *rawNode
}

type rawTreeParser struct {
	data []byte
	pos  int
}

func parseRawTree(data []byte) *rawNode {
	p := &rawTreeParser{data: data}
	return p.value(nil)
}

func (p *rawTreeParser) ws() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *rawTreeParser) value(parent *rawNode) *rawNode {
	p.ws()
	if p.pos >= len(p.data) {
		return nil
	}
	n := &rawNode{data: p.data, kind: p.data[p.pos], start: p.pos, parent: parent}
	switch n.kind {
	case '{':
		p.pos++
		n.tokEnd = p.pos
		for {
			p.ws()
			if p.pos >= len(p.data) {
				return nil
			}
			if p.data[p.pos] == '}' {
				p.pos++
				break
			}
			w := &rawJSONWalker{data: p.data, pos: p.pos}
			key, ok := w.readString()
			if !ok {
				return nil
			}
			p.pos = w.pos
			p.ws()
			if p.pos >= len(p.data) || p.data[p.pos] != ':' {
				return nil
			}
			p.pos++
			child := p.value(n)
			if child == nil {
				return nil
			}
			n.keys = append(n.keys, key)
			n.vals = append(n.vals, child)
			p.ws()
			if p.pos < len(p.data) && p.data[p.pos] == ',' {
				p.pos++
			}
		}
	case '[':
		p.pos++
		n.tokEnd = p.pos
		for {
			p.ws()
			if p.pos >= len(p.data) {
				return nil
			}
			if p.data[p.pos] == ']' {
				p.pos++
				break
			}
			child := p.value(n)
			if child == nil {
				return nil
			}
			n.vals = append(n.vals, child)
			p.ws()
			if p.pos < len(p.data) && p.data[p.pos] == ',' {
				p.pos++
			}
		}
	case '"':
		w := &rawJSONWalker{data: p.data, pos: p.pos}
		if _, ok := w.readString(); !ok {
			return nil
		}
		p.pos = w.pos
		n.tokEnd = p.pos
	default:
		for p.pos < len(p.data) && !strings.ContainsRune(",]} \t\r\n", rune(p.data[p.pos])) {
			p.pos++
		}
		if p.pos == n.start {
			return nil
		}
		n.tokEnd = p.pos
	}
	n.end = p.pos
	return n
}

// value decodes the JSON of a node (numbers as json.Number, dotted keys
// kept as they are).
func (n *rawNode) value() any {
	if n == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(n.data[n.start:n.end]))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}

// leavesAt lists the values found at a field path in document order. Arrays
// are flattened like the document parser does, except that whole keeps an
// array found at the path as one value; inArray reports values that are
// elements of an array.
func (n *rawNode) leavesAt(parts []string, whole bool) (nodes []*rawNode, inArray []bool) {
	var flatten func(v *rawNode)
	flatten = func(v *rawNode) {
		if v.kind == '[' {
			for _, e := range v.vals {
				flatten(e)
			}
			return
		}
		nodes = append(nodes, v)
		inArray = append(inArray, true)
	}
	var walk func(v *rawNode, parts []string)
	walk = func(v *rawNode, parts []string) {
		if v == nil {
			return
		}
		if len(parts) == 0 {
			if v.kind == '[' && !whole {
				for _, e := range v.vals {
					flatten(e)
				}
				return
			}
			nodes = append(nodes, v)
			inArray = append(inArray, false)
			return
		}
		switch v.kind {
		case '{':
			for i, k := range v.keys {
				kp := splitJavaPath(k)
				if len(kp) > len(parts) {
					continue
				}
				match := true
				for j := range kp {
					if kp[j] != parts[j] {
						match = false
						break
					}
				}
				if match {
					walk(v.vals[i], parts[len(kp):])
				}
			}
		case '[':
			for _, e := range v.vals {
				walk(e, parts)
			}
		}
	}
	walk(n, parts)
	return nodes, inArray
}

// rawTree returns the parsed JSON of the document being built: the request
// body when known (where OpenSearch locates its failures), else the stored
// source.
func (b *docBuilder) rawTree() *rawNode {
	root := b.rootBuilder()
	if root.tree == nil && root.src != nil {
		if raw := root.locationSource(); len(raw) > 0 {
			root.tree = parseRawTree(raw)
		}
	}
	return root.tree
}

// lineCol is Jackson's location of the first token of a node.
func (n *rawNode) lineCol() (int, int) {
	if n == nil {
		return 1, 0
	}
	return jacksonLineCol(n.data, n.start)
}

// rawLeaf returns the occ-th value at a field path of the source (nil when
// it cannot be located) and whether it is an array element.
func (b *docBuilder) rawLeaf(path string, occ int, whole bool) (*rawNode, bool) {
	tree := b.rawTree()
	if tree == nil {
		return nil, false
	}
	nodes, inArray := tree.leavesAt(strings.Split(path, "."), whole)
	if occ < 0 || occ >= len(nodes) {
		return nil, false
	}
	return nodes[occ], inArray[occ]
}

// valueTokenName is the XContentParser.Token name of a value.
func valueTokenName(v any) string {
	switch v.(type) {
	case nil:
		return "VALUE_NULL"
	case bool:
		return "VALUE_BOOLEAN"
	case string:
		return "VALUE_STRING"
	case json.Number, float64, float32, int, int64:
		return "VALUE_NUMBER"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	}
	return "VALUE_EMBEDDED_OBJECT"
}

// rawTokenName is the XContentParser.Token name of the token starting at
// a byte of the source.
func rawTokenName(c byte, inObject bool) string {
	switch c {
	case '{':
		return "START_OBJECT"
	case '[':
		return "START_ARRAY"
	case '}':
		return "END_OBJECT"
	case ']':
		return "END_ARRAY"
	case '"':
		if inObject {
			return "FIELD_NAME"
		}
		return "VALUE_STRING"
	case 't', 'f':
		return "VALUE_BOOLEAN"
	case 'n':
		return "VALUE_NULL"
	}
	return "VALUE_NUMBER"
}

// errLeftoverContent is the failure of a document whose parser was left
// inside an object by a swallowed mapping failure: the object's content is
// read as the rest of the enclosing object and the document ends early.
func (b *docBuilder) errLeftoverContent(node *rawNode) *Error {
	token := "END_OBJECT"
	if node != nil {
		var chain []*rawNode
		for n := node; n != nil && n.parent != nil; n = n.parent {
			chain = append(chain, n)
		}
		for i := len(chain) - 1; i >= 0; i-- {
			if chain[i].kind != '{' {
				continue
			}
			a := chain[i]
			p := a.end
			skip := func() {
				for p < len(a.data) && strings.IndexByte(" \t\r\n", a.data[p]) >= 0 {
					p++
				}
			}
			skip()
			inObject := a.parent.kind == '{'
			if p < len(a.data) && a.data[p] == ',' {
				p++
				skip()
				if p < len(a.data) {
					token = rawTokenName(a.data[p], inObject)
				}
			} else if p < len(a.data) {
				token = rawTokenName(a.data[p], false)
			}
			break
		}
	}
	return errFailedToParse(errIllegalArgument("Malformed content, found extra data after parsing: %s", token))
}

// inputCoercionAt is a Jackson coercion failure at a byte offset.
func inputCoercionAt(token string, node *rawNode) *Error {
	off := 0
	if node != nil {
		off = node.tokEnd
	}
	reason := fmt.Sprintf("Current token (%s) not numeric, cannot use numeric value accessors", jacksonTokenName(token)) + jacksonLocation(off)
	return &Error{Type: "input_coercion_exception", Reason: reason, Cause: &Error{Type: "input_coercion_exception", Reason: reason}}
}

// jacksonTokenName maps an XContent token name to Jackson's JsonToken name.
func jacksonTokenName(token string) string {
	if token == "VALUE_BOOLEAN" {
		return "VALUE_TRUE"
	}
	return token
}

func jacksonValueToken(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "VALUE_TRUE"
		}
		return "VALUE_FALSE"
	}
	return valueTokenName(v)
}

// indexIgnoreMalformed is the index.mapping.ignore_malformed setting.
func (ix *Index) indexIgnoreMalformed() bool {
	return getBool(getMap(getMap(ix.Settings, "index"), "mapping"), "ignore_malformed", false)
}

// swallowsFailures reports the mappers without an ignore_malformed
// parameter whose parse failures FieldMapper.parse drops (without _ignored)
// when index.mapping.ignore_malformed is set.
func (ix *Index) swallowsFailures(f *Field) bool {
	switch f.Type {
	case TypeKeyword, TypeText, TypeMatchOnlyText, TypeSearchAsYouType, TypeWildcard, TypeBoolean, TypeConstantKeyword,
		TypeTokenCount, TypeVersion, TypeFlatObject, TypeRankFeature:
		return ix.indexIgnoreMalformed()
	}
	return false
}

// knn_vector --------------------------------------------------------------------

func knnFloat(v float32, name string) *Error {
	switch {
	case math.IsNaN(float64(v)):
		return errIllegalArgument("KNN vector values cannot be NaN")
	case math.IsInf(float64(v), 0):
		return errIllegalArgument("KNN vector values cannot be infinity")
	}
	return nil
}

// addKNNVector validates a vector (KNNVectorFieldMapper): an array of
// numbers, a single number or a base64 string of big-endian floats.
func (b *docBuilder) addKNNVector(name, rawPath string, f *Field, v any, occ int) (bool, error) {
	dimension := getInt(f.Extra, "dimension", 0)
	byteType := false
	switch strings.ToLower(getString(f.Extra, "data_type")) {
	case "byte":
		byteType = true
	case "binary":
		byteType = true
		dimension /= 8
	}
	fail := func(preview string, cause *Error) (bool, error) {
		if b.ix.indexIgnoreMalformed() {
			return false, &Error{Status: 500, Type: "illegal_state_exception", Reason: "found leftover path elements: " + name + "."}
		}
		return false, b.valueError(name, f, v, preview, cause)
	}
	element := func(e any, node *rawNode) (string, *Error) {
		var fv float32
		preview := ""
		switch t := e.(type) {
		case json.Number:
			d, _ := strconv.ParseFloat(t.String(), 64)
			fv = float32(d)
			if isIntToken(t.String()) {
				preview = javaJSONNumberText(t)
			} else {
				preview = javaFloatText(float64(fv), 32)
			}
		case string:
			d, err := javaParseDouble(t, 32)
			if err != nil {
				return t, err
			}
			fv = float32(d)
			preview = t
		default:
			return javaValueString(e), inputCoercionAt(jacksonValueToken(e), node)
		}
		if err := knnFloat(fv, name); err != nil {
			return preview, err
		}
		if byteType {
			if float64(fv) != math.Trunc(float64(fv)) {
				return preview, errIllegalArgument("[data_type] field was set as [byte] in index mapping. But, KNN vector values are floats instead of byte integers")
			}
			if fv < -128 || fv > 127 {
				return preview, errIllegalArgument("[data_type] field was set as [byte] in index mapping. But, KNN vector values are not within in the byte range [-128, 127]")
			}
		}
		return preview, nil
	}
	node, _ := b.rawLeaf(rawPath, occ, true)
	count := 0
	preview := "null"
	switch t := v.(type) {
	case []any:
		for i, e := range t {
			var en *rawNode
			if node != nil && node.kind == '[' && i < len(node.vals) {
				en = node.vals[i]
			}
			if p, err := element(e, en); err != nil {
				return fail(p, err)
			}
			count++
		}
	case json.Number:
		if p, err := element(t, node); err != nil {
			return fail(p, err)
		}
		count = 1
	case string:
		preview = t
		data, err := javaBase64Decode(t)
		if err != nil {
			return fail(t, err)
		}
		if len(data)%4 != 0 {
			return fail(t, errIllegalArgument("Base64 encoded vector for field [%s] has invalid byte length [%d], must be a multiple of 4 (float size)", name, len(data)))
		}
		for i := 0; i+4 <= len(data); i += 4 {
			bits := uint32(data[i])<<24 | uint32(data[i+1])<<16 | uint32(data[i+2])<<8 | uint32(data[i+3])
			if err := knnFloat(math.Float32frombits(bits), name); err != nil {
				return fail(t, err)
			}
			count++
		}
	default:
		preview = javaValueString(v)
	}
	if count != dimension {
		return fail(preview, errIllegalArgument("Vector dimension mismatch. Expected: %d, Given: %d", dimension, count))
	}
	return true, nil
}

// javaBase64Decode is Base64.getDecoder().decode (padding optional).
func javaBase64Decode(s string) ([]byte, *Error) {
	trimmed := strings.TrimRight(s, "=")
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return nil, errIllegalArgument("Illegal base64 character %x", c)
		}
	}
	data, err := base64.RawStdEncoding.DecodeString(trimmed)
	if err != nil {
		if len(trimmed)%4 == 1 {
			return nil, errIllegalArgument("Last unit does not have enough valid bits")
		}
		return nil, errIllegalArgument("%s", err.Error())
	}
	return data, nil
}

// completion ------------------------------------------------------------------------

var completionContentFields = map[string]bool{"input": true, "weight": true, "contexts": true}

// addCompletion parses the value of a completion field (arrays included)
// and indexes each input as the term of its analyzed tokens.
func (b *docBuilder) addCompletion(name, rawPath string, f *Field, v any, occ int) (bool, error) {
	node, inArray := b.rawLeaf(rawPath, occ, true)
	simple := rawPath[strings.LastIndexByte(rawPath, '.')+1:]
	if inArray {
		simple = "null"
	}
	var inputs []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			inputs = append(inputs, s)
		}
	}
	parseOne := func(e any, en *rawNode, current string) *Error {
		switch t := e.(type) {
		case string:
			add(t)
			return nil
		case M:
			keys := sortedMapKeys(t)
			var vals []*rawNode
			if en != nil && en.kind == '{' {
				keys, vals = en.keys, en.vals
			}
			for i, k := range keys {
				if !completionContentFields[k] {
					return errIllegalArgument("unknown field name [%s], must be one of [input, weight, contexts]", k)
				}
				val := t[k]
				if vals != nil {
					val = vals[i].value()
				}
				switch k {
				case "input":
					switch iv := val.(type) {
					case string:
						add(iv)
					case []any:
						for _, item := range iv {
							s, ok := item.(string)
							if !ok {
								return errIllegalArgument("input array must have string values, but was [%s]", valueTokenName(item))
							}
							add(s)
						}
					default:
						return errIllegalArgument("input must be a string or array, but was [%s]", valueTokenName(val))
					}
				case "weight":
					var weight int64
					switch wv := val.(type) {
					case string:
						n, err := strconv.ParseInt(strings.TrimPrefix(wv, "+"), 10, 64)
						if err != nil || strings.HasPrefix(wv, "+-") {
							return errIllegalArgument("weight must be an integer, but was [%s]", wv)
						}
						weight = n
					case json.Number:
						n, err := strconv.ParseInt(wv.String(), 10, 64)
						if err != nil {
							return errIllegalArgument("weight must be an integer, but was [%s]", javaJSONNumberText(wv))
						}
						weight = n
					default:
						return errIllegalArgument("weight must be a number or string, but was [%s]", valueTokenName(val))
					}
					if weight < 0 || weight > math.MaxInt32 {
						return errIllegalArgument("weight must be in the interval [0..2147483647], but was [%d]", weight)
					}
				case "contexts":
					if f.Extra["contexts"] == nil {
						return errIllegalArgument("contexts field is not supported for field: [%s]", name)
					}
				}
			}
			return nil
		}
		line, col := en.lineCol()
		return &Error{Status: 400, Type: "parsing_exception", Reason: fmt.Sprintf("failed to parse [%s]: expected text or object, but got %s", current, valueTokenName(e)),
			Extra: map[string]any{"line": line, "col": col}}
	}
	if arr, ok := v.([]any); ok {
		for i, e := range arr {
			var en *rawNode
			if node != nil && node.kind == '[' && i < len(node.vals) {
				en = node.vals[i]
			}
			if err := parseOne(e, en, "null"); err != nil {
				return false, errFailedToParse(err)
			}
		}
	} else if err := parseOne(v, node, simple); err != nil {
		return false, errFailedToParse(err)
	}
	analyzerName := f.Analyzer
	if analyzerName == "" {
		analyzerName = "simple"
	}
	an, err := b.ix.analysis.analyzerNamed(analyzerName)
	if err != nil {
		return false, err
	}
	maxLength := getInt(f.Extra, "max_input_length", 50)
	for _, input := range javaHashMapOrder(inputs) {
		if strings.TrimFunc(input, func(r rune) bool { return r <= ' ' }) == "" {
			b.rootBuilder().ignored[name] = true
			continue
		}
		units := utf16.Encode([]rune(input))
		if len(units) > maxLength {
			n := maxLength
			if n > 0 && utf16.IsSurrogate(rune(units[n-1])) && units[n-1] < 0xdc00 {
				n++
			}
			units = units[:n]
			input = string(utf16.Decode(units))
		}
		for i, u := range units {
			if u == 0x1f || u == 0x1e || u == 0 {
				return false, errFailedToParse(errIllegalArgument("Illegal input [%s] UTF-16 codepoint [0x%x] at position %d is a reserved character", input, u, i))
			}
		}
		term := strings.Join(tokens(an, input), "\x1f")
		b.doc.AddField(document.NewTextFieldCustom(name, nil, []byte(term), index.IndexField|index.IncludeTermVectors, b.ix.keywordAnalyzer()))
	}
	return true, nil
}

// flat_object -----------------------------------------------------------------------

// addFlatObject indexes the leaves of a flat_object value: every value as a
// keyword of the field and of its dotted path below the field.
func (b *docBuilder) addFlatObject(name, rawPath string, f *Field, v any, arrayPos []uint64, occ int) (bool, error) {
	obj, ok := v.(M)
	if !ok {
		if b.ix.swallowsFailures(f) {
			return false, nil
		}
		node, _ := b.rawLeaf(rawPath, occ, false)
		line, col := node.lineCol()
		cause := &Error{Status: 400, Type: "parsing_exception", Reason: fmt.Sprintf("[%s] unexpected token [%s] in flat_object field value", name, valueTokenName(v)),
			Extra: map[string]any{"line": line, "col": col}}
		return false, b.valueError(name, f, v, "", cause)
	}
	opts := index.IndexField | index.IncludeTermVectors
	kw := b.ix.keywordAnalyzer()
	limit := getInt(f.Extra, "ignore_above", math.MaxInt32)
	indexed := false
	var walk func(path string, val any)
	walk = func(path string, val any) {
		switch t := val.(type) {
		case M:
			for _, k := range sortedMapKeys(t) {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(p, t[k])
			}
		case []any:
			for _, e := range t {
				walk(path, e)
			}
		case nil:
		default:
			var text string
			switch x := t.(type) {
			case string:
				text = x
			case json.Number:
				text = x.String()
			case bool:
				text = strconv.FormatBool(x)
			case float64:
				text = strconv.FormatFloat(x, 'f', -1, 64)
			default:
				text = fmt.Sprint(x)
			}
			if len(utf16.Encode([]rune(text))) > limit {
				return
			}
			b.doc.AddField(document.NewTextFieldCustom(name, arrayPos, []byte(text), opts, kw))
			if path != "" {
				full := name + "." + path
				b.doc.AddField(document.NewTextFieldCustom(full, arrayPos, []byte(text), opts, kw))
				b.exists[full] = true
			}
			indexed = true
		}
	}
	walk("", obj)
	return indexed, nil
}

// rank_feature and rank_features -------------------------------------------------

const minNormalFloat32 = 1.17549435e-38

func featureValueError(value float32, feature, field string) *Error {
	if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
		return errIllegalArgument("featureValue must be finite, got: %s for feature %s on field %s", javaFloatText(float64(value), 32), feature, field)
	}
	if value < minNormalFloat32 {
		return errIllegalArgument("featureValue must be a positive normal float, got: %s for feature %s on field %s which is less than the minimum positive normal float: 1.1754944E-38", javaFloatText(float64(value), 32), feature, field)
	}
	return nil
}

// addRankFeature validates a rank_feature value (RankFeatureFieldMapper).
func (b *docBuilder) addRankFeature(name, rawPath string, f *Field, v any, occ int) (bool, error) {
	fail := func(preview string, cause *Error) (bool, error) {
		if b.ix.swallowsFailures(f) {
			return false, nil
		}
		return false, b.valueError(name, f, v, preview, cause)
	}
	var value float32
	preview := ""
	switch t := v.(type) {
	case json.Number:
		d, _ := strconv.ParseFloat(t.String(), 64)
		value = float32(d)
		if !isIntToken(t.String()) {
			preview = javaFloatText(float64(value), 32)
		}
	case string:
		d, err := javaParseDouble(t, 32)
		if err != nil {
			return fail("", err)
		}
		value = float32(d)
	default:
		node, _ := b.rawLeaf(rawPath, occ, false)
		return fail("", inputCoercionAt(jacksonValueToken(v), node))
	}
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	if b.seen[name] {
		return fail(preview, errIllegalArgument("[rank_feature] fields do not support indexing multiple values for the same field [%s] in the same document", name))
	}
	if !getBool(f.Extra, "positive_score_impact", true) {
		value = 1 / value
	}
	if err := featureValueError(value, name, "_feature"); err != nil {
		return fail(preview, err)
	}
	b.seen[name] = true
	return true, nil
}

// addRankFeatures validates a rank_features object (RankFeaturesFieldMapper).
func (b *docBuilder) addRankFeatures(name, rawPath string, f *Field, v any, occ int) (bool, error) {
	obj, ok := v.(M)
	if !ok {
		return false, errFailedToParse(errIllegalArgument("[rank_features] fields must be json objects, expected a START_OBJECT but got: %s", valueTokenName(v)))
	}
	node, _ := b.rawLeaf(rawPath, occ, false)
	keys := sortedMapKeys(obj)
	var vals []*rawNode
	if node != nil && node.kind == '{' {
		keys, vals = node.keys, node.vals
	}
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	for i, k := range keys {
		var val any
		if vals != nil {
			val = vals[i].value()
		} else {
			val = obj[k]
		}
		var value float32
		switch t := val.(type) {
		case nil:
			continue
		case json.Number:
			d, _ := strconv.ParseFloat(t.String(), 64)
			value = float32(d)
		case string:
			d, err := javaParseDouble(t, 32)
			if err != nil {
				return false, errFailedToParse(err)
			}
			value = float32(d)
		default:
			return false, errFailedToParse(errIllegalArgument("[rank_features] fields take hashes that map a feature to a strictly positive float, but got unexpected token %s", valueTokenName(val)))
		}
		key := name + "." + k
		if b.seen[key] {
			return false, errFailedToParse(errIllegalArgument("[rank_features] fields do not support indexing multiple values for the same rank feature [%s] in the same document", key))
		}
		if !getBool(f.Extra, "positive_score_impact", true) {
			value = 1 / value
		}
		if err := featureValueError(value, k, name); err != nil {
			return false, errFailedToParse(err)
		}
		b.seen[key] = true
	}
	return true, nil
}

// join ------------------------------------------------------------------------------

// joinRelationsOf returns the parents of a join field in declaration order
// and the children of each parent.
func joinRelationsOf(f *Field) ([]string, map[string][]string) {
	rel, _ := f.Extra["relations"].(M)
	parents := javaHashMapOrder(sortedMapKeys(rel))
	children := map[string][]string{}
	for _, p := range parents {
		switch t := rel[p].(type) {
		case []any:
			for _, c := range t {
				children[p] = append(children[p], javaValueString(c))
			}
		case nil:
		default:
			children[p] = []string{javaValueString(t)}
		}
	}
	return parents, children
}

// normalizeJoinRelations checks the relations of a join field and renders
// them the way the mapper does (a single child as a string, several in
// hash set order).
func normalizeJoinRelations(name string, f *Field) *Error {
	raw, ok := f.Extra["relations"]
	if !ok {
		return nil
	}
	rel, ok := raw.(M)
	if !ok {
		return classCastToMap(raw)
	}
	parentOf := map[string]string{}
	var conflicts []string
	out := M{}
	for _, p := range javaHashMapOrder(sortedMapKeys(rel)) {
		var kids []string
		switch t := rel[p].(type) {
		case []any:
			for _, c := range t {
				kids = append(kids, javaValueString(c))
			}
		default:
			kids = []string{javaValueString(t)}
		}
		for _, c := range kids {
			if _, dup := parentOf[c]; dup {
				conflicts = append(conflicts, "["+c+"] cannot have multiple parents")
			}
			parentOf[c] = p
		}
		out[p] = joinChildrenJSON(kids)
	}
	if len(conflicts) > 0 {
		return errIllegalArgument("invalid definition for join field [%s]:\n[%s]", name, strings.Join(conflicts, ", "))
	}
	f.Extra["relations"] = out
	return nil
}

func joinChildrenJSON(kids []string) any {
	uniq := map[string]bool{}
	var list []string
	for _, c := range kids {
		if !uniq[c] {
			uniq[c] = true
			list = append(list, c)
		}
	}
	if len(list) == 1 {
		return list[0]
	}
	out := make([]any, 0, len(list))
	for _, c := range javaHashMapOrder(list) {
		out = append(out, c)
	}
	return out
}

// mergeJoinRelations applies the relations of a PUT _mapping to a join
// field (ParentJoinFieldMapper.mergeOptions).
func mergeJoinRelations(f, nf *Field, full string) error {
	oldParents, oldChildren := joinRelationsOf(f)
	newParents, newChildren := joinRelationsOf(nf)
	isOldParent := map[string]bool{}
	isOldChild := map[string]bool{}
	for _, p := range oldParents {
		isOldParent[p] = true
		for _, c := range oldChildren[p] {
			isOldChild[c] = true
		}
	}
	isNewParent := map[string]bool{}
	for _, p := range newParents {
		isNewParent[p] = true
	}
	var conflicts []string
	for _, p := range oldParents {
		if !isNewParent[p] {
			conflicts = append(conflicts, fmt.Sprintf("cannot remove parent [%s] in join field [%s]", p, full))
		}
	}
	merged := M{}
	for _, p := range newParents {
		if isOldParent[p] {
			kept := map[string]bool{}
			for _, c := range newChildren[p] {
				kept[c] = true
			}
			for _, c := range oldChildren[p] {
				if !kept[c] {
					conflicts = append(conflicts, fmt.Sprintf("cannot remove child [%s] in join field [%s]", c, full))
				}
			}
		} else {
			if isOldChild[p] {
				conflicts = append(conflicts, fmt.Sprintf("cannot create parent [%s] from an existing child", p))
			}
			for _, c := range newChildren[p] {
				if isOldParent[c] {
					conflicts = append(conflicts, fmt.Sprintf("cannot create child [%s] from an existing parent", c))
				}
			}
		}
		merged[p] = joinChildrenJSON(newChildren[p])
	}
	if len(conflicts) > 0 {
		return errIllegalArgument("Mapper for [%s] conflicts with existing mapping:\n[%s]", full, strings.Join(conflicts, ", "))
	}
	if f.Extra == nil {
		f.Extra = M{}
	}
	f.Extra["relations"] = merged
	if v, ok := nf.Extra["eager_global_ordinals"]; ok {
		f.Extra["eager_global_ordinals"] = v
	} else {
		delete(f.Extra, "eager_global_ordinals")
	}
	return nil
}

// addJoin parses the value of a join field (ParentJoinFieldMapper.parse):
// the relation name, and the parent id of child documents.
func (b *docBuilder) addJoin(name, rawPath string, f *Field, v any, occ int) (bool, error) {
	var relName, parent *string
	switch t := v.(type) {
	case string:
		relName = &t
	case M:
		node, _ := b.rawLeaf(rawPath, occ, false)
		var failure *Error
		var visit func(key string, val any)
		visit = func(key string, val any) {
			if failure != nil {
				return
			}
			switch x := val.(type) {
			case string:
				switch key {
				case "name":
					s := x
					relName = &s
				case "parent":
					s := x
					parent = &s
				default:
					failure = errIllegalArgument("unknown field name [%s] in join field [%s]", key, name)
				}
			case json.Number:
				if key == "parent" {
					s := javaJSONNumberText(x)
					parent = &s
				} else {
					failure = errIllegalArgument("unknown field name [%s] in join field [%s]", key, name)
				}
			case []any:
				for _, e := range x {
					visit(key, e)
				}
			case M:
				for _, k := range sortedMapKeys(x) {
					visit(k, x[k])
				}
			}
		}
		keys := sortedMapKeys(t)
		var vals []*rawNode
		if node != nil && node.kind == '{' {
			keys, vals = node.keys, node.vals
		}
		for i, k := range keys {
			val := t[k]
			if vals != nil {
				val = vals[i].value()
			}
			visit(k, val)
		}
		if failure != nil {
			return false, errFailedToParse(failure)
		}
	default:
		return false, errFailedToParse(&Error{Type: "illegal_state_exception", Reason: fmt.Sprintf("[null] expected START_OBJECT or VALUE_STRING but was: %s", valueTokenName(v))})
	}
	if relName == nil {
		return false, errFailedToParse(&Error{Type: "null_pointer_exception", Reason: "Cannot invoke \"String.equals(Object)\" because \"name\" is null"})
	}
	parents, children := joinRelationsOf(f)
	isParent := false
	childOf := ""
	for _, p := range parents {
		if p == *relName {
			isParent = true
		}
		for _, c := range children[p] {
			if c == *relName && childOf == "" {
				childOf = p
			}
		}
	}
	if !isParent && childOf == "" {
		return false, errFailedToParse(errIllegalArgument("unknown join name [%s] for field [%s]", *relName, name))
	}
	opts := index.IndexField | index.IncludeTermVectors
	kw := b.ix.keywordAnalyzer()
	if childOf != "" {
		if parent == nil {
			return false, errFailedToParse(errIllegalArgument("[parent] is missing for join field [%s]", name))
		}
		if docRouting(b.src) == "" {
			return false, errFailedToParse(errIllegalArgument("[routing] is missing for join field [%s]", name))
		}
		b.doc.AddField(document.NewTextFieldCustom(name+"#"+childOf, nil, []byte(*parent), opts, kw))
	}
	if isParent {
		b.doc.AddField(document.NewTextFieldCustom(name+"#"+*relName, nil, []byte(b.src.ID), opts, kw))
	}
	b.doc.AddField(document.NewTextFieldCustom(name, nil, []byte(*relName), opts, kw))
	return true, nil
}

// joinParentField resolves the parent id field of a join relation
// ("my_join#question").
func (m *Mapping) joinParentField(path string) *Field {
	hash := strings.LastIndexByte(path, '#')
	if hash <= 0 {
		return nil
	}
	jf, ok := m.Properties[path[:hash]]
	if !ok || jf.Type != TypeJoin {
		return nil
	}
	parents, _ := joinRelationsOf(jf)
	for _, p := range parents {
		if p == path[hash+1:] {
			return &Field{Type: TypeKeyword, Index: true, Enabled: true, Extra: M{}}
		}
	}
	return nil
}

// field data --------------------------------------------------------------------

// fielddataUnsupported is the failure of loading field data (sorts,
// aggregations, docvalue_fields) of fields that have none.
func fielddataUnsupported(field string, f *Field) *Error {
	if f == nil {
		return nil
	}
	switch f.Type {
	case TypeIgnoredMeta, TypeCompletion, TypeSearchAsYouType:
		return errIllegalArgument("Fielddata is not supported on field [%s] of type [%s]", field, f.typeName())
	}
	return nil
}

// metadata fields in exists queries ---------------------------------------------

// metaFieldNames are the metadata field types of OpenSearch 3.8 in
// registration order (the order of names sharing a hash bucket when a field
// pattern expands to them).
var metaFieldNames = []string{"_id", "_index", "_routing", "_seq_no", "_version", "_source", "_feature", "_field_names", "_ignored",
	"_nested_path", "_data_stream_timestamp", "_doc_count"}

func isMetaFieldName(name string) bool {
	for _, m := range metaFieldNames {
		if m == name {
			return true
		}
	}
	return false
}

// metaExistsQuery is the exists query of a metadata field.
func (qb *queryBuilder) metaExistsQuery(name string) (query.Query, *Error) {
	switch name {
	case "_feature":
		return nil, errCreateQuery("unsupported_operation_exception", "Cannot run exists query on [_feature]")
	case "_nested_path":
		return nil, errCreateQuery("unsupported_operation_exception", "Cannot run exists() query against the nested field path")
	case "_field_names":
		return nil, errCreateQuery("unsupported_operation_exception", "Cannot run exists query on _field_names")
	case "_data_stream_timestamp":
		return nil, errCreateQuery("unsupported_operation_exception", "Cannot run exists query on internal field [_data_stream_timestamp]")
	case "_source":
		return nil, errQueryShard("The _source field is not searchable")
	case "_id", "_index", "_seq_no", "_version":
		return bleve.NewMatchAllQuery(), nil
	case "_ignored":
		return fieldPresenceQuery("_ignored"), nil
	case "_routing":
		ix := qb.ix
		return &docFuncQuery{inner: bleve.NewMatchAllQuery(), fn: func(id string, score float64) (float64, bool) {
			d := ix.docByBleveID(id)
			return score, d != nil && d.nested == nil && docRouting(d) != ""
		}}, nil
	}
	return nil, nil
}
