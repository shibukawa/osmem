package engine

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Template is an index template (composable or legacy).
type Template struct {
	Name     string
	Patterns []string
	// Priority is the priority of a composable template (0 when unset) or
	// the order of a legacy template.
	Priority int
	Settings M // normalized {"index": {...}}
	Mappings M
	Aliases  M // alias definitions as given
	Version  any
	Legacy   bool

	// composable templates only
	HasPriority   bool
	ComposedOf    []string
	HasComposedOf bool
	HasTemplate   bool
	HasSettings   bool
	HasMappings   bool
	HasAliases    bool
	Meta          M
	HasMeta       bool
	DataStream    M // rendered data_stream object; nil when unset
}

// ComponentTemplate is a component template (/_component_template).
type ComponentTemplate struct {
	Name        string
	Settings    M
	Mappings    M
	Aliases     M
	HasSettings bool
	HasMappings bool
	HasAliases  bool
	Version     any
	Meta        M
	HasMeta     bool
}

func (t *Template) matches(name string) bool {
	for _, p := range t.Patterns {
		if wildcardMatch(p, name) {
			return true
		}
	}
	return false
}

// errors and validation ------------------------------------------------------

func errInvalidIndexTemplate(name, cause string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_index_template_exception", Reason: "index_template [" + name + "] invalid, cause [" + cause + "]"}
}

func validationFailed(messages []string) string {
	var sb strings.Builder
	sb.WriteString("Validation Failed: ")
	for i, m := range messages {
		fmt.Fprintf(&sb, "%d: %s;", i+1, m)
	}
	return sb.String()
}

// templateNameErrors are the name and pattern checks of
// MetadataIndexTemplateService.validate.
func templateNameErrors(name string, patterns []string) []string {
	var errs []string
	if strings.Contains(name, " ") {
		errs = append(errs, "name must not contain a space")
	}
	if strings.Contains(name, ",") {
		errs = append(errs, "name must not contain a ','")
	}
	if strings.Contains(name, "#") {
		errs = append(errs, "name must not contain a '#'")
	}
	if strings.Contains(name, "*") {
		errs = append(errs, "name must not contain a '*'")
	}
	if strings.HasPrefix(name, "_") {
		errs = append(errs, "name must not start with '_'")
	}
	if strings.ToLower(name) != name {
		errs = append(errs, "name must be lower cased")
	}
	for _, p := range patterns {
		if strings.Contains(p, " ") {
			errs = append(errs, "index_patterns ["+p+"] must not contain a space")
		}
		if strings.Contains(p, ",") {
			errs = append(errs, "index_pattern ["+p+"] must not contain a ','")
		}
		if strings.Contains(p, "#") {
			errs = append(errs, "index_pattern ["+p+"] must not contain a '#'")
		}
		if strings.Contains(p, ":") {
			errs = append(errs, "index_pattern ["+p+"] must not contain a ':'")
		}
		if strings.HasPrefix(p, "_") {
			errs = append(errs, "index_pattern ["+p+"] must not start with '_'")
		}
		if strings.ContainsAny(p, "\\/?\"<>| ,") {
			errs = append(errs, "index_pattern ["+p+"] must not contain the following characters [ , \", *, \\, <, |, ,, >, /, ?]")
		}
	}
	return errs
}

// templateSettingsErrors validates composable template settings: when the
// first failure is an unknown setting it is returned as is (a
// SettingsException), otherwise the failures become validation messages.
func templateSettingsErrors(flat map[string]any) (*Error, []string) {
	var messages []string
	for _, key := range sortedFlatKeys(flat) {
		def, known := lookupIndexSetting(key)
		if !known {
			if len(messages) == 0 {
				return errUnknownIndexSetting(key), nil
			}
			messages = append(messages, errUnknownIndexSetting(key).Reason)
			continue
		}
		if err := validateSetting(key, def, flat[key]); err != nil {
			messages = append(messages, err.Reason)
		}
	}
	for _, key := range sortedFlatKeys(flat) {
		if def, _ := lookupIndexSetting(key); def.private() && flat[key] != nil {
			messages = append(messages, "private index setting ["+key+"] can not be set explicitly")
		}
	}
	return nil, messages
}

// firstTemplateSettingError is the settings check of validateTemplate used
// by component and legacy templates: the first failure is returned as is.
func firstTemplateSettingError(flat map[string]any) error {
	for _, key := range sortedFlatKeys(flat) {
		def, known := lookupIndexSetting(key)
		if !known {
			return errUnknownIndexSetting(key)
		}
		if err := validateSetting(key, def, flat[key]); err != nil {
			return err
		}
	}
	return nil
}

func mappingParseError(err error) *Error {
	cause, ok := err.(*Error)
	if !ok {
		cause = &Error{Type: "exception", Reason: err.Error()}
	}
	return &Error{Status: http.StatusBadRequest, Type: "mapper_parsing_exception", Reason: "Failed to parse mapping [_doc]: " + cause.Reason, Cause: cause}
}

// parseTemplateLong parses priority and version fields.
func parseTemplateLong(owner string, m M, field string) (int64, *Error) {
	switch t := m[field].(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), nil
		}
	case float64:
		return int64(t), nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, xContentErr("["+owner+"] failed to parse field ["+field+"]", javaIllegalArgument(`For input string: "`+t+`"`)).at(valueTok(m, field))
		}
		return n, nil
	}
	return 0, xContentErr("["+owner+"] "+field+" doesn't support values of type: "+xContentToken(m[field]), nil).at(valueTok(m, field))
}

// templateSection is the parsed "template" object of composable and
// component templates.
type templateSection struct {
	settings                             M
	flat                                 map[string]any
	mappings                             M
	aliases                              M
	hasSettings, hasMappings, hasAliases bool
}

func parseTemplateSection(raw M) (*templateSection, *Error) {
	s := &templateSection{settings: M{"index": M{}}, flat: map[string]any{}, mappings: M{}, aliases: M{}}
	for _, key := range sortedMapKeys(raw) {
		v := raw[key]
		m, isObject := v.(M)
		switch key {
		case "settings", "mappings", "aliases":
			if !isObject {
				return nil, xContentErr("[template] "+key+" doesn't support values of type: "+xContentToken(v), nil).at(valueTok(raw, key))
			}
		default:
			return nil, xContentErr("[template] unknown field ["+key+"]", nil).at(keyTok(raw, key)).atParser(valueTok(raw, key))
		}
		switch key {
		case "settings":
			flattenSettingsBody(s.flat, "", m)
			s.flat = normalizeIndexSettingKeys(s.flat)
			s.settings = nestSettings(s.flat)
			if _, ok := s.settings["index"]; !ok {
				s.settings["index"] = M{}
			}
			s.hasSettings = true
		case "mappings":
			if doc, typed := m["_doc"].(M); typed && len(m) == 1 {
				m = doc
			}
			s.mappings = m
			s.hasMappings = true
		case "aliases":
			s.aliases = m
			s.hasAliases = true
		}
	}
	return s, nil
}

// composable templates ---------------------------------------------------------

func parseComposableTemplate(name string, body M) (*Template, error) {
	t := &Template{Name: name, Settings: M{"index": M{}}, Mappings: M{}, Aliases: M{}}
	for _, key := range sortedMapKeys(body) {
		switch key {
		case "index_patterns", "template", "composed_of", "priority", "version", "_meta", "data_stream":
		default:
			return nil, xContentErr("[index_template] unknown field ["+key+"]", nil).at(keyTok(body, key)).atParser(valueTok(body, key))
		}
	}
	for _, key := range sortedMapKeys(body) {
		v := body[key]
		switch key {
		case "index_patterns":
			list, ok := stringListStrict(v)
			if !ok {
				return nil, xContentErr("[index_template] index_patterns doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key))
			}
			t.Patterns = list
		case "template":
			m, ok := v.(M)
			if !ok {
				return nil, xContentErr("[index_template] template doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key))
			}
			section, err := parseTemplateSection(m)
			if err != nil {
				return nil, xContentErr("[index_template] failed to parse field [template]", err).atCause(valueEndTok(body, key))
			}
			t.HasTemplate = true
			t.Settings, t.Mappings, t.Aliases = section.settings, section.mappings, section.aliases
			t.HasSettings, t.HasMappings, t.HasAliases = section.hasSettings, section.hasMappings, section.hasAliases
		case "composed_of":
			list, ok := stringListStrict(v)
			if !ok {
				return nil, xContentErr("[index_template] composed_of doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key))
			}
			t.ComposedOf = list
			t.HasComposedOf = true
		case "priority":
			n, err := parseTemplateLong("index_template", body, key)
			if err != nil {
				return nil, err
			}
			t.Priority = int(n)
			t.HasPriority = true
		case "version":
			n, err := parseTemplateLong("index_template", body, key)
			if err != nil {
				return nil, err
			}
			t.Version = n
		case "_meta":
			m, ok := v.(M)
			if !ok {
				return nil, xContentErr("[index_template] _meta doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key))
			}
			t.Meta = m
			t.HasMeta = true
		case "data_stream":
			m, ok := v.(M)
			if !ok {
				return nil, xContentErr("[index_template] data_stream doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key))
			}
			field := "@timestamp"
			if tf, ok := m["timestamp_field"].(M); ok {
				if n, ok := tf["name"].(string); ok && n != "" {
					field = n
				}
			}
			t.DataStream = M{"timestamp_field": M{"name": field}}
		}
	}
	if len(t.Patterns) == 0 {
		return nil, javaIllegalArgument("Required [index_patterns]")
	}
	return t, nil
}

// OpenSearch 3.8 deliberately uses stripped-pattern matching rather than
// full language intersection (which would reject logs* together with *2026).
func templatePatternsOverlap(a, b string) bool {
	return wildcardMatch(a, strings.ReplaceAll(b, "*", "")) ||
		wildcardMatch(b, strings.ReplaceAll(a, "*", ""))
}

func patternsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if templatePatternsOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

// conflictingV2Templates lists the other composable templates whose patterns
// overlap (with the same priority when checkPriority is set).
func (c *Cluster) conflictingV2Templates(name string, patterns []string, checkPriority bool, priority int) map[string][]string {
	out := map[string][]string{}
	for otherName, other := range c.templates {
		if otherName == name || !patternsOverlap(patterns, other.Patterns) {
			continue
		}
		if checkPriority && other.Priority != priority {
			continue
		}
		out[otherName] = other.Patterns
	}
	return out
}

func (c *Cluster) conflictingV1Templates(patterns []string) map[string][]string {
	out := map[string][]string{}
	for name, t := range c.legacyTemplates {
		if patternsOverlap(patterns, t.Patterns) {
			out[name] = t.Patterns
		}
	}
	return out
}

// validateComposableTemplate runs the checks of addIndexTemplateV2 before a
// composable template is stored.
func (c *Cluster) validateComposableTemplate(t *Template, create bool) error {
	if _, exists := c.templates[t.Name]; create && exists {
		return javaIllegalArgument("index template [" + t.Name + "] already exists")
	}
	if overlaps := c.conflictingV2Templates(t.Name, t.Patterns, true, t.Priority); len(overlaps) > 0 {
		names := make([]string, 0, len(overlaps))
		for n := range overlaps {
			names = append(names, n)
		}
		names = javaHashSetOrder(names)
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = n + " => " + javaList(overlaps[n])
		}
		return javaIllegalArgument(fmt.Sprintf("index template [%s] has index patterns %s matching patterns from existing templates [%s] with patterns (%s) that have the same priority [%d], multiple index templates may not match during index creation, please use a different priority",
			t.Name, javaList(t.Patterns), strings.Join(names, ","), strings.Join(parts, ","), t.Priority))
	}
	var missing []string
	for _, ct := range t.ComposedOf {
		if _, ok := c.componentTemplates[ct]; !ok {
			missing = append(missing, ct)
		}
	}
	if len(missing) > 0 {
		return errInvalidIndexTemplate(t.Name, "index template ["+t.Name+"] specifies component templates "+javaList(missing)+" that do not exist")
	}
	errs := templateNameErrors(t.Name, t.Patterns)
	unknown, messages := templateSettingsErrors(flatIndexSettings(t.Settings))
	if unknown != nil {
		return unknown
	}
	errs = append(errs, messages...)
	if len(errs) > 0 {
		return errInvalidIndexTemplate(t.Name, validationFailed(errs))
	}
	if err := validateTemplateAliasNames(t.Aliases); err != nil {
		return err
	}
	return c.validateComposition(t)
}

func validateTemplateAliasNames(aliases M) error {
	defs, err := parseAliasObjects(aliases)
	if err != nil {
		return err
	}
	for _, d := range defs {
		if err := validateAliasName(d.name, d.effectiveIndexRouting()); err != nil {
			return err
		}
	}
	return nil
}

// validateComposition checks that the template composed with its component
// templates yields valid mappings and alias filters.
func (c *Cluster) validateComposition(t *Template) error {
	wrap := func(cause *Error) error {
		return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "composable template [" + t.Name + "] template after composition is invalid", Cause: cause}
	}
	flat, mappings, aliasLists := c.composeTemplate(t)
	mp, err := parseMapping(mappings)
	if err != nil {
		cause, ok := err.(*Error)
		if !ok {
			cause = &Error{Type: "exception", Reason: err.Error()}
		}
		return wrap(&Error{Type: "illegal_argument_exception", Reason: "invalid composite mappings for [" + t.Name + "]", Cause: mappingParseError(cause)})
	}
	var filtered []*aliasDef
	for _, list := range aliasLists {
		for _, d := range list {
			if len(d.filter) > 0 {
				filtered = append(filtered, d)
			}
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	for key, def := range map[string]string{"index.number_of_shards": "1", "index.number_of_replicas": "0"} {
		if _, ok := flat[key]; !ok {
			flat[key] = def
		}
	}
	ix, err := newIndex("validate_composable_template", nestSettings(flat), mp, c.now(), nil)
	if err != nil {
		return nil
	}
	defer ix.release()
	for _, d := range filtered {
		if err := c.validateAliasFilter(ix, d.name, d.filter); err != nil {
			return wrap(err.(*Error))
		}
	}
	return nil
}

// composeTemplate resolves a composable template with its component
// templates: settings and mappings of the component templates in order,
// then the template's own; alias lists with the template's aliases first
// (earlier lists win).
func (c *Cluster) composeTemplate(t *Template) (map[string]any, M, [][]*aliasDef) {
	flat := map[string]any{}
	mappings := M{}
	var aliasLists [][]*aliasDef
	for _, name := range t.ComposedOf {
		ct, ok := c.componentTemplates[name]
		if !ok {
			continue
		}
		for k, v := range flatIndexSettings(ct.Settings) {
			flat[k] = v
		}
		mergeTemplateMappings(mappings, ct.Mappings)
		if defs, err := parseAliasObjects(ct.Aliases); err == nil {
			aliasLists = append(aliasLists, defs)
		}
	}
	for k, v := range flatIndexSettings(t.Settings) {
		flat[k] = v
	}
	mergeTemplateMappings(mappings, t.Mappings)
	if defs, err := parseAliasObjects(t.Aliases); err == nil {
		aliasLists = append(aliasLists, defs)
	}
	for i, j := 0, len(aliasLists)-1; i < j; i, j = i+1, j-1 {
		aliasLists[i], aliasLists[j] = aliasLists[j], aliasLists[i]
	}
	return flat, mappings, aliasLists
}

// mergeTemplateMappings merges mappings the way MergeReason.INDEX_TEMPLATE
// does: object fields merge recursively, leaf fields are replaced.
func mergeTemplateMappings(dst, src M) {
	for k, v := range src {
		sp, isProps := v.(M)
		if k != "properties" || !isProps {
			dst[k] = cloneDeep(v)
			continue
		}
		dp, ok := dst["properties"].(M)
		if !ok {
			dp = M{}
			dst["properties"] = dp
		}
		for field, def := range sp {
			sf, sObj := def.(M)
			df, dObj := dp[field].(M)
			if sObj && dObj && isObjectFieldMapping(sf) && isObjectFieldMapping(df) {
				mergeTemplateMappings(df, sf)
				continue
			}
			dp[field] = cloneDeep(def)
		}
	}
}

func isObjectFieldMapping(f M) bool {
	if _, ok := f["properties"]; ok {
		return true
	}
	typ, _ := f["type"].(string)
	return typ == "" || typ == "object" || typ == "nested"
}

// matchingComposableTemplate is findV2Template: the matching composable
// template with the highest priority.
func (c *Cluster) matchingComposableTemplate(index string) *Template {
	var names []string
	for name, t := range c.templates {
		if t.matches(index) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	names = javaHashSetOrder(names)
	sort.SliceStable(names, func(i, j int) bool { return c.templates[names[i]].Priority > c.templates[names[j]].Priority })
	return c.templates[names[0]]
}

// PutIndexTemplate implements PUT /_index_template/{name}.
func (c *Cluster) PutIndexTemplate(name string, body M, p Params) (Response, error) {
	t, err := parseComposableTemplate(name, body)
	if err != nil {
		return fail(err)
	}
	if t.HasPriority && t.Priority < 0 {
		return fail(errActionRequestValidation("index template priority must be >= 0"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validateComposableTemplate(t, p.Bool("create", false)); err != nil {
		return fail(err)
	}
	c.templates[name] = t
	return ok(M{"acknowledged": true})
}

func renderTemplateAliases(aliases M, legacy bool) M {
	out := M{}
	defs, _ := parseAliasObjects(aliases)
	for _, d := range defs {
		if legacy {
			d.isWriteIndex, d.isHidden = nil, nil
		}
		out[d.name] = aliasDefJSON(d)
	}
	return out
}

func composableTemplateJSON(t *Template) M {
	out := M{"index_patterns": t.Patterns}
	if t.HasTemplate {
		tmpl := M{}
		if t.HasSettings {
			tmpl["settings"] = renderSettings(flatIndexSettings(t.Settings), false)
		}
		if t.HasMappings {
			tmpl["mappings"] = t.Mappings
		}
		if t.HasAliases {
			tmpl["aliases"] = renderTemplateAliases(t.Aliases, false)
		}
		out["template"] = tmpl
		composed := t.ComposedOf
		if composed == nil {
			composed = []string{}
		}
		out["composed_of"] = composed
	} else if t.HasComposedOf {
		out["composed_of"] = t.ComposedOf
	}
	if t.HasPriority {
		out["priority"] = t.Priority
	}
	if t.Version != nil {
		out["version"] = t.Version
	}
	if t.HasMeta {
		out["_meta"] = t.Meta
	}
	if t.DataStream != nil {
		out["data_stream"] = t.DataStream
	}
	return out
}

// GetIndexTemplate implements GET /_index_template/{name}.
func (c *Cluster) GetIndexTemplate(name string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var names []string
	switch {
	case name == "":
		for n := range c.templates {
			names = append(names, n)
		}
	case strings.Contains(name, "*"):
		for n := range c.templates {
			if simpleMatch(name, n) {
				names = append(names, n)
			}
		}
	default:
		if _, ok := c.templates[name]; !ok {
			return fail(&Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "index template matching [" + name + "] not found"})
		}
		names = []string{name}
	}
	list := []any{}
	for _, n := range javaHashSetOrder(names) {
		list = append(list, M{"name": n, "index_template": composableTemplateJSON(c.templates[n])})
	}
	status := http.StatusOK
	if name != "" && len(list) == 0 {
		status = http.StatusNotFound
	}
	return Response{Status: status, Body: M{"index_templates": list}}, nil
}

// IndexTemplateExists implements HEAD /_index_template/{name}.
func (c *Cluster) IndexTemplateExists(name string) (Response, error) {
	r, err := c.GetIndexTemplate(name, Params{})
	if err != nil {
		return Response{Status: http.StatusNotFound}, nil
	}
	return Response{Status: r.Status}, nil
}

// DeleteIndexTemplate implements DELETE /_index_template/{name}.
func (c *Cluster) DeleteIndexTemplate(name string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var matched []string
	for n := range c.templates {
		if simpleMatch(name, n) {
			matched = append(matched, n)
		}
	}
	if len(matched) == 0 {
		if name == "*" {
			return ok(M{"acknowledged": true})
		}
		return fail(&Error{Status: http.StatusNotFound, Type: "index_template_missing_exception", Reason: "index_template [" + name + "] missing"})
	}
	for _, n := range matched {
		delete(c.templates, n)
	}
	return ok(M{"acknowledged": true})
}

// simulation -------------------------------------------------------------------

func simulateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

// simulationResponse renders the resolved template and the templates that
// overlap with it (SimulateIndexTemplateResponse).
func (c *Cluster) simulationResponse(t *Template, index string, p Params) M {
	flat, mappings, aliasLists := c.composeTemplate(t)
	aliases := M{}
	for _, list := range aliasLists {
		for _, d := range list {
			aliasName := strings.ReplaceAll(d.name, "{index}", index)
			if _, seen := aliases[aliasName]; !seen {
				aliases[aliasName] = aliasDefJSON(d)
			}
		}
	}
	tmpl := M{"settings": renderSettings(flat, p.Bool("flat_settings", false)), "aliases": aliases}
	if len(mappings) > 0 {
		tmpl["mappings"] = mappings
	}
	overlaps := c.conflictingV1Templates(t.Patterns)
	for n, pats := range c.conflictingV2Templates(t.Name, t.Patterns, false, 0) {
		overlaps[n] = pats
	}
	names := make([]string, 0, len(overlaps))
	for n := range overlaps {
		names = append(names, n)
	}
	overlapping := []any{}
	for _, n := range javaHashSetOrder(names) {
		overlapping = append(overlapping, M{"name": n, "index_patterns": overlaps[n]})
	}
	return M{"template": tmpl, "overlapping": overlapping}
}

// SimulateIndexTemplate implements POST /_index_template/_simulate[/{name}]
// with or without a template body.
func (c *Cluster) SimulateIndexTemplate(name string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	index := "simulate_template_index_" + simulateUUID()
	if len(body) == 0 {
		if name == "" {
			return fail(errActionRequestValidation("either index name or index template body must be specified for simulation"))
		}
		t, exists := c.templates[name]
		if !exists {
			return fail(javaIllegalArgument("unable to simulate template [" + name + "] that does not exist"))
		}
		return ok(c.simulationResponse(t, index, p))
	}
	templateName := name
	if templateName == "" {
		templateName = "simulate_template_" + simulateUUID()
	}
	t, err := parseComposableTemplate(templateName, body)
	if err != nil {
		return fail(err)
	}
	if t.HasPriority && t.Priority < 0 {
		return fail(errActionRequestValidation("index template priority must be >= 0"))
	}
	if err := c.validateComposableTemplate(t, p.Bool("create", false)); err != nil {
		return fail(err)
	}
	return ok(c.simulationResponse(t, index, p))
}

// SimulateIndexTemplateForIndex implements POST
// /_index_template/_simulate_index/{index}.
func (c *Cluster) SimulateIndexTemplateForIndex(index string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t := c.matchingComposableTemplate(index)
	if t == nil {
		return ok(M{})
	}
	return ok(c.simulationResponse(t, index, p))
}

// component templates -------------------------------------------------------

// PutComponentTemplate implements PUT /_component_template/{name}.
func (c *Cluster) PutComponentTemplate(name string, body M, p Params) (Response, error) {
	ct := &ComponentTemplate{Name: name, Settings: M{"index": M{}}, Mappings: M{}, Aliases: M{}}
	hasTemplate := false
	for _, key := range sortedMapKeys(body) {
		v := body[key]
		switch key {
		case "template":
			m, ok := v.(M)
			if !ok {
				return fail(xContentErr("[component_template] template doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key)))
			}
			section, err := parseTemplateSection(m)
			if err != nil {
				return fail(xContentErr("[component_template] failed to parse field [template]", err).atCause(valueEndTok(body, key)))
			}
			hasTemplate = true
			ct.Settings, ct.Mappings, ct.Aliases = section.settings, section.mappings, section.aliases
			ct.HasSettings, ct.HasMappings, ct.HasAliases = section.hasSettings, section.hasMappings, section.hasAliases
		case "version":
			n, err := parseTemplateLong("component_template", body, key)
			if err != nil {
				return fail(err)
			}
			ct.Version = n
		case "_meta":
			m, ok := v.(M)
			if !ok {
				return fail(xContentErr("[component_template] _meta doesn't support values of type: "+xContentToken(v), nil).at(valueTok(body, key)))
			}
			ct.Meta = m
			ct.HasMeta = true
		default:
			return fail(xContentErr("[component_template] unknown field ["+key+"]", nil).at(keyTok(body, key)).atParser(valueTok(body, key)))
		}
	}
	if !hasTemplate {
		return fail(javaIllegalArgument("Required [template]"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.componentTemplates[name]; exists && p.Bool("create", false) {
		return fail(javaIllegalArgument("component template [" + name + "] already exists"))
	}
	if err := firstTemplateSettingError(flatIndexSettings(ct.Settings)); err != nil {
		return fail(err)
	}
	if _, err := parseMapping(ct.Mappings); err != nil {
		return fail(mappingParseError(err))
	}
	if errs := templateNameErrors(name, nil); len(errs) > 0 {
		return fail(errInvalidIndexTemplate(name, validationFailed(errs)))
	}
	if err := validateTemplateAliasNames(ct.Aliases); err != nil {
		return fail(err)
	}
	c.componentTemplates[name] = ct
	return ok(M{"acknowledged": true})
}

func componentTemplateJSON(ct *ComponentTemplate) M {
	tmpl := M{}
	if ct.HasSettings {
		tmpl["settings"] = renderSettings(flatIndexSettings(ct.Settings), false)
	}
	if ct.HasMappings {
		tmpl["mappings"] = ct.Mappings
	}
	if ct.HasAliases {
		tmpl["aliases"] = renderTemplateAliases(ct.Aliases, false)
	}
	out := M{"template": tmpl}
	if ct.Version != nil {
		out["version"] = ct.Version
	}
	if ct.HasMeta {
		out["_meta"] = ct.Meta
	}
	return out
}

// GetComponentTemplate implements GET /_component_template[/{name}].
func (c *Cluster) GetComponentTemplate(name string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var names []string
	switch {
	case name == "":
		for n := range c.componentTemplates {
			names = append(names, n)
		}
	case strings.Contains(name, "*"):
		for n := range c.componentTemplates {
			if simpleMatch(name, n) {
				names = append(names, n)
			}
		}
	default:
		if _, ok := c.componentTemplates[name]; !ok {
			return fail(&Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "component template matching [" + name + "] not found"})
		}
		names = []string{name}
	}
	list := []any{}
	for _, n := range javaHashSetOrder(names) {
		list = append(list, M{"name": n, "component_template": componentTemplateJSON(c.componentTemplates[n])})
	}
	status := http.StatusOK
	if name != "" && len(list) == 0 {
		status = http.StatusNotFound
	}
	return Response{Status: status, Body: M{"component_templates": list}}, nil
}

// ComponentTemplateExists implements HEAD /_component_template/{name}.
func (c *Cluster) ComponentTemplateExists(name string) (Response, error) {
	r, err := c.GetComponentTemplate(name, Params{})
	if err != nil {
		return Response{Status: http.StatusNotFound}, nil
	}
	return Response{Status: r.Status}, nil
}

// DeleteComponentTemplate implements DELETE /_component_template/{name}.
func (c *Cluster) DeleteComponentTemplate(name string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var matched []string
	for n := range c.componentTemplates {
		if simpleMatch(name, n) {
			matched = append(matched, n)
		}
	}
	if len(matched) == 0 {
		if name == "*" {
			return ok(M{"acknowledged": true})
		}
		return fail(&Error{Status: http.StatusNotFound, Type: "index_template_missing_exception", Reason: "index_template [" + name + "] missing"})
	}
	inUse := map[string]bool{}
	var users []string
	for tn, t := range c.templates {
		uses := false
		for _, ct := range t.ComposedOf {
			for _, m := range matched {
				if ct == m {
					inUse[m] = true
					uses = true
				}
			}
		}
		if uses {
			users = append(users, tn)
		}
	}
	if len(users) > 0 {
		var used []string
		for _, m := range matched {
			if inUse[m] {
				used = append(used, m)
			}
		}
		return fail(javaIllegalArgument("component templates " + javaList(javaHashSetOrder(used)) + " cannot be removed as they are still in use by index templates " + javaList(javaHashSetOrder(users))))
	}
	for _, n := range matched {
		delete(c.componentTemplates, n)
	}
	return ok(M{"acknowledged": true})
}

// legacy templates ---------------------------------------------------------------

func templateClassCastToMap(v any) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: "class_cast_exception", Reason: "class " + adminJavaClassName(v) + " cannot be cast to class java.util.Map (" + adminJavaClassName(v) + " and java.util.Map are in module java.base of loader 'bootstrap')"}
}

// PutLegacyTemplate implements PUT /_template/{name}
// (PutIndexTemplateRequest.source and MetadataIndexTemplateService.putTemplate).
func (c *Cluster) PutLegacyTemplate(name string, body M, p Params) (Response, error) {
	t := &Template{Name: name, Legacy: true, Settings: M{"index": M{}}, Mappings: M{}, Aliases: M{}}
	if p.Has("order") {
		n, err := strconv.ParseInt(p.Get("order"), 10, 32)
		if err != nil {
			return fail(&Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Failed to parse int parameter [order] with value [" + p.Get("order") + "]",
				Cause: &Error{Type: "number_format_exception", Reason: `For input string: "` + p.Get("order") + `"`}})
		}
		t.Priority = int(n)
	}
	flat := map[string]any{}
	for _, key := range sortedMapKeys(body) {
		v := body[key]
		switch key {
		case "template":
			if s, ok := v.(string); ok && len(t.Patterns) == 0 {
				t.Patterns = []string{s}
			}
		case "index_patterns":
			switch pv := v.(type) {
			case string:
				t.Patterns = []string{pv}
			case []any:
				t.Patterns = nil
				for _, e := range pv {
					t.Patterns = append(t.Patterns, settingString(e))
				}
			default:
				return fail(javaIllegalArgument("Malformed [index_patterns] value, should be a string or a list of strings"))
			}
		case "order":
			switch ov := v.(type) {
			case json.Number:
				f, _ := ov.Float64()
				t.Priority = int(f)
			case string:
				n, err := strconv.ParseInt(ov, 10, 32)
				if err != nil {
					return fail(&Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: `For input string: "` + ov + `"`})
				}
				t.Priority = int(n)
			}
		case "version":
			n, isNumber := v.(json.Number)
			if !isNumber || strings.ContainsAny(n.String(), ".eE") {
				return fail(javaIllegalArgument("Malformed [version] value, should be an integer"))
			}
			i, _ := n.Int64()
			t.Version = i
		case "settings":
			m, ok := v.(M)
			if !ok {
				return fail(javaIllegalArgument("Malformed [settings] section, should include an inner object"))
			}
			flattenSettingsBody(flat, "", m)
			flat = normalizeIndexSettingKeys(flat)
			t.Settings = nestSettings(flat)
			if _, ok := t.Settings["index"]; !ok {
				t.Settings["index"] = M{}
			}
		case "mappings":
			m, ok := v.(M)
			if !ok {
				return fail(templateClassCastToMap(v))
			}
			if _, typed := m["_doc"].(M); typed && len(m) == 1 {
				return fail(javaIllegalArgument("The mapping definition cannot be nested under a type"))
			}
			t.Mappings = m
		case "aliases":
			m, ok := v.(M)
			if !ok {
				return fail(templateClassCastToMap(v))
			}
			t.Aliases = m
		default:
			return fail(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "unknown key [" + key + "] in the template "})
		}
	}
	if len(t.Patterns) == 0 {
		return fail(errActionRequestValidation("index patterns are missing"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.legacyTemplates[name]; exists && p.Bool("create", false) {
		return fail(javaIllegalArgument("index_template [" + name + "] already exists"))
	}
	if err := firstTemplateSettingError(flat); err != nil {
		return fail(err)
	}
	if _, err := parseMapping(t.Mappings); err != nil {
		return fail(mappingParseError(err))
	}
	if errs := templateNameErrors(name, t.Patterns); len(errs) > 0 {
		return fail(errInvalidIndexTemplate(name, validationFailed(errs)))
	}
	if err := validateTemplateAliasNames(t.Aliases); err != nil {
		return fail(err)
	}
	c.legacyTemplates[name] = t
	return ok(M{"acknowledged": true})
}

// GetLegacyTemplate implements GET /_template/{name}.
func (c *Cluster) GetLegacyTemplate(name string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	patterns := splitList(name)
	out := M{}
	for n, t := range c.legacyTemplates {
		if len(patterns) > 0 {
			matched := false
			for _, pat := range patterns {
				if (strings.Contains(pat, "*") && simpleMatch(pat, n)) || pat == n {
					matched = true
				}
			}
			if !matched {
				continue
			}
		}
		body := M{"order": t.Priority, "index_patterns": t.Patterns,
			"settings": renderSettings(flatIndexSettings(t.Settings), p.Bool("flat_settings", false)),
			"mappings": t.Mappings, "aliases": renderTemplateAliases(t.Aliases, true)}
		if t.Version != nil {
			body["version"] = t.Version
		}
		out[n] = body
	}
	if len(out) == 0 && len(patterns) > 0 {
		return Response{Status: http.StatusNotFound, Body: M{}}, nil
	}
	return ok(out)
}

// LegacyTemplateExists implements HEAD /_template/{name}.
func (c *Cluster) LegacyTemplateExists(name string) (Response, error) {
	r, _ := c.GetLegacyTemplate(name, Params{})
	return Response{Status: r.Status}, nil
}

// DeleteLegacyTemplate implements DELETE /_template/{name}.
func (c *Cluster) DeleteLegacyTemplate(name string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for n := range c.legacyTemplates {
		if simpleMatch(name, n) {
			delete(c.legacyTemplates, n)
			found = true
		}
	}
	if !found && name != "*" {
		return fail(&Error{Status: http.StatusNotFound, Type: "index_template_missing_exception", Reason: "index_template [" + name + "] missing"})
	}
	return ok(M{"acknowledged": true})
}

// data streams ---------------------------------------------------------------------

// errDataStreamsUnsupported rejects data stream creation: osmem keeps and
// renders templates with a data_stream section but has no data streams.
func errDataStreamsUnsupported() *Error {
	return &Error{Status: http.StatusBadRequest, Type: "unsupported_operation_exception", Reason: "data streams are not supported by osmem"}
}

// CreateDataStream implements PUT /_data_stream/{name}.
func (c *Cluster) CreateDataStream(name string) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t := c.matchingComposableTemplate(name)
	if t == nil {
		return fail(javaIllegalArgument("no matching index template found for data stream [" + name + "]"))
	}
	if t.DataStream == nil {
		return fail(javaIllegalArgument("matching index template [" + t.Name + "] for data stream [" + name + "] has no data stream template"))
	}
	return fail(errDataStreamsUnsupported())
}

// GetDataStream implements GET /_data_stream[/{name}]; there never are any.
func (c *Cluster) GetDataStream(name string) (Response, error) {
	if name != "" && name != "_all" && !strings.Contains(name, "*") {
		return fail(errIndexNotFound(name))
	}
	return ok(M{"data_streams": []any{}})
}

// DeleteDataStream implements DELETE /_data_stream/{name}.
func (c *Cluster) DeleteDataStream(name string) (Response, error) {
	if name != "_all" && !strings.Contains(name, "*") {
		return fail(errIndexNotFound(name))
	}
	return ok(M{"acknowledged": true})
}

// DataStreamStats implements GET /_data_stream[/{name}]/_stats.
func (c *Cluster) DataStreamStats(name string) (Response, error) {
	if name != "" && name != "_all" && !strings.Contains(name, "*") {
		return fail(errIndexNotFound(name))
	}
	return ok(M{"_shards": M{"total": 0, "successful": 0, "failed": 0}, "data_stream_count": 0, "backing_indices": 0, "total_store_size_bytes": 0, "data_streams": []any{}})
}
