// GraphQL executor for the 18 conformance documents: a small lexer,
// parser, and executor over the shared store. Supports variables,
// aliases, inline and named fragments, object/list/enum arguments, the
// __type introspection probe, node(id:), and the five mutations.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---- lexer --------------------------------------------------------------

type gqlToken struct {
	kind string // "{","}","(",")","[","]",":","!","$","...","=","@","|","&","name","int","float","string","eof"
	text string
}

func lexGraphQL(src string) []gqlToken {
	var out []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			out = append(out, gqlToken{"...", "..."})
			i += 3
		case strings.IndexByte("{}()[]:!@$|&=", c) >= 0:
			out = append(out, gqlToken{string(c), string(c)})
			i++
		case c == '"':
			i++
			var sb strings.Builder
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' && i+1 < len(src) {
					i++
					switch src[i] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case 'u':
						if i+4 < len(src) {
							n, _ := strconv.ParseUint(src[i+1:i+5], 16, 32)
							sb.WriteRune(rune(n))
							i += 4
						}
					default:
						sb.WriteByte(src[i])
					}
					i++
					continue
				}
				sb.WriteByte(src[i])
				i++
			}
			i++ // closing quote
			out = append(out, gqlToken{"string", sb.String()})
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			out = append(out, gqlToken{kind, src[i:j]})
			i = j
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			j := i
			for j < len(src) && ((src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			out = append(out, gqlToken{"name", src[i:j]})
			i = j
		default:
			i++ // skip anything unexpected
		}
	}
	out = append(out, gqlToken{"eof", ""})
	return out
}

// ---- parser -------------------------------------------------------------

// gqlValue is a parsed literal (variables unresolved).
type gqlValue struct {
	kind string // "var","null","bool","int","float","string","enum","list","object"
	s    string
	b    bool
	i    int64
	f    float64
	list []gqlValue
	obj  []gqlObjField
}

type gqlObjField struct {
	name string
	val  gqlValue
}

type gqlArg struct {
	name string
	val  gqlValue
}

type gqlSelection struct {
	kind     string // "field","spread","inline"
	alias    string
	name     string
	args     []gqlArg
	sels     []gqlSelection
	fragName string
	typeCond string
}

func (s gqlSelection) outName() string {
	if s.alias != "" {
		return s.alias
	}
	return s.name
}

type gqlOperation struct {
	kind string // "query" or "mutation"
	name string
	vars map[string]gqlValue // variable defaults
	sels []gqlSelection
}

type gqlFragment struct {
	typeCond string
	sels     []gqlSelection
}

type gqlDoc struct {
	ops   []gqlOperation
	frags map[string]gqlFragment
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind string) (gqlToken, error) {
	t := p.next()
	if t.kind != kind {
		return t, fmt.Errorf("expected %s, got %s %q", kind, t.kind, t.text)
	}
	return t, nil
}

func parseGraphQL(src string) (*gqlDoc, error) {
	p := &gqlParser{toks: lexGraphQL(src)}
	doc := &gqlDoc{frags: map[string]gqlFragment{}}
	for p.peek().kind != "eof" {
		t := p.peek()
		switch {
		case t.kind == "{":
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, gqlOperation{kind: "query", vars: map[string]gqlValue{}, sels: sels})
		case t.kind == "name" && (t.text == "query" || t.text == "mutation"):
			op, err := p.parseOperation()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, op)
		case t.kind == "name" && t.text == "fragment":
			if err := p.parseFragment(doc); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unexpected token %s %q", t.kind, t.text)
		}
	}
	return doc, nil
}

func (p *gqlParser) parseOperation() (gqlOperation, error) {
	kind := p.next().text // query | mutation
	op := gqlOperation{kind: kind, vars: map[string]gqlValue{}}
	if p.peek().kind == "name" {
		op.name = p.next().text
	}
	if p.peek().kind == "(" {
		p.next()
		for p.peek().kind != ")" && p.peek().kind != "eof" {
			if _, err := p.expect("$"); err != nil {
				return op, err
			}
			name, err := p.expect("name")
			if err != nil {
				return op, err
			}
			if _, err := p.expect(":"); err != nil {
				return op, err
			}
			// type: Name, [Type], Type! — consume tokens until , ) or =
			for {
				t := p.peek()
				if t.kind == "name" || t.kind == "[" || t.kind == "]" || t.kind == "!" {
					p.next()
					continue
				}
				break
			}
			var def gqlValue
			if p.peek().kind == "=" {
				p.next()
				v, err := p.parseValue()
				if err != nil {
					return op, err
				}
				def = v
			}
			op.vars[name.text] = def
		}
		if _, err := p.expect(")"); err != nil {
			return op, err
		}
	}
	sels, err := p.parseSelections()
	if err != nil {
		return op, err
	}
	op.sels = sels
	return op, nil
}

func (p *gqlParser) parseFragment(doc *gqlDoc) error {
	p.next() // fragment
	name, err := p.expect("name")
	if err != nil {
		return err
	}
	if t := p.next(); t.text != "on" {
		return fmt.Errorf("expected 'on' in fragment, got %q", t.text)
	}
	cond, err := p.expect("name")
	if err != nil {
		return err
	}
	sels, err := p.parseSelections()
	if err != nil {
		return err
	}
	doc.frags[name.text] = gqlFragment{typeCond: cond.text, sels: sels}
	return nil
}

func (p *gqlParser) parseSelections() ([]gqlSelection, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	var sels []gqlSelection
	for p.peek().kind != "}" && p.peek().kind != "eof" {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if _, err := p.expect("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	t := p.peek()
	if t.kind == "..." {
		p.next()
		if p.peek().kind == "name" && p.peek().text == "on" {
			p.next()
			cond, err := p.expect("name")
			if err != nil {
				return gqlSelection{}, err
			}
			sels, err := p.parseSelections()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "inline", typeCond: cond.text, sels: sels}, nil
		}
		name, err := p.expect("name")
		if err != nil {
			return gqlSelection{}, err
		}
		return gqlSelection{kind: "spread", fragName: name.text}, nil
	}
	name, err := p.expect("name")
	if err != nil {
		return gqlSelection{}, err
	}
	sel := gqlSelection{kind: "field", name: name.text}
	if p.peek().kind == ":" {
		p.next()
		alias, err := p.expect("name")
		if err != nil {
			return gqlSelection{}, err
		}
		sel.alias = sel.name
		sel.name = alias.text
	}
	if p.peek().kind == "(" {
		p.next()
		for p.peek().kind != ")" && p.peek().kind != "eof" {
			argName, err := p.expect("name")
			if err != nil {
				return gqlSelection{}, err
			}
			if _, err := p.expect(":"); err != nil {
				return gqlSelection{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlSelection{}, err
			}
			sel.args = append(sel.args, gqlArg{argName.text, v})
		}
		if _, err := p.expect(")"); err != nil {
			return gqlSelection{}, err
		}
	}
	// directives: skip @name(args)
	for p.peek().kind == "@" {
		p.next()
		if _, err := p.expect("name"); err != nil {
			return gqlSelection{}, err
		}
		if p.peek().kind == "(" {
			p.next()
			depth := 1
			for depth > 0 && p.peek().kind != "eof" {
				t := p.next()
				if t.kind == "(" {
					depth++
				} else if t.kind == ")" {
					depth--
				}
			}
		}
	}
	if p.peek().kind == "{" {
		sels, err := p.parseSelections()
		if err != nil {
			return gqlSelection{}, err
		}
		sel.sels = sels
	}
	return sel, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	t := p.next()
	switch t.kind {
	case "$":
		name, err := p.expect("name")
		if err != nil {
			return gqlValue{}, err
		}
		return gqlValue{kind: "var", s: name.text}, nil
	case "int":
		n, _ := strconv.ParseInt(t.text, 10, 64)
		return gqlValue{kind: "int", i: n}, nil
	case "float":
		f, _ := strconv.ParseFloat(t.text, 64)
		return gqlValue{kind: "float", f: f}, nil
	case "string":
		return gqlValue{kind: "string", s: t.text}, nil
	case "name":
		switch t.text {
		case "true":
			return gqlValue{kind: "bool", b: true}, nil
		case "false":
			return gqlValue{kind: "bool", b: false}, nil
		case "null":
			return gqlValue{kind: "null"}, nil
		}
		return gqlValue{kind: "enum", s: t.text}, nil
	case "[":
		var list []gqlValue
		for p.peek().kind != "]" && p.peek().kind != "eof" {
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			list = append(list, v)
		}
		if _, err := p.expect("]"); err != nil {
			return gqlValue{}, err
		}
		return gqlValue{kind: "list", list: list}, nil
	case "{":
		var obj []gqlObjField
		for p.peek().kind != "}" && p.peek().kind != "eof" {
			name, err := p.expect("name")
			if err != nil {
				return gqlValue{}, err
			}
			if _, err := p.expect(":"); err != nil {
				return gqlValue{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			obj = append(obj, gqlObjField{name.text, v})
		}
		if _, err := p.expect("}"); err != nil {
			return gqlValue{}, err
		}
		return gqlValue{kind: "object", obj: obj}, nil
	}
	return gqlValue{}, fmt.Errorf("unexpected value token %s %q", t.kind, t.text)
}

// ---- argument resolution -----------------------------------------------

// resolveValue substitutes variables and normalises JSON numbers.
func (e *gqlExec) resolveValue(v gqlValue) any {
	switch v.kind {
	case "var":
		if val, ok := e.vars[v.s]; ok {
			return normalizeJSON(val)
		}
		return nil
	case "null":
		return nil
	case "bool":
		return v.b
	case "int":
		return v.i
	case "float":
		return v.f
	case "string", "enum":
		return v.s
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, e.resolveValue(item))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range v.obj {
			out[f.name] = e.resolveValue(f.val)
		}
		return out
	}
	return nil
}

func normalizeJSON(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSON(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeJSON(val)
		}
		return t
	}
	return v
}

func (e *gqlExec) resolveArgs(args []gqlArg) map[string]any {
	out := map[string]any{}
	for _, a := range args {
		out[a.name] = e.resolveValue(a.val)
	}
	return out
}

func argStr(args map[string]any, name string) string {
	if v, ok := args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]any, name string, def int) int {
	if v, ok := args[name]; ok {
		switch t := v.(type) {
		case int64:
			return int(t)
		case float64:
			return int(t)
		case int:
			return t
		}
	}
	return def
}

func argStrList(args map[string]any, name string) []string {
	v, ok := args[name]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---- executor -----------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

type gqlExec struct {
	s     *server
	vars  map[string]any
	frags map[string]gqlFragment
	errs  []gqlError
}

type gqlObject interface {
	gqlType() string
	gqlField(e *gqlExec, name string, args map[string]any) (any, error)
}

func gqlErrField(name, typ string) error {
	return fmt.Errorf("Cannot query field %q on type %q.", name, typ)
}

func (e *gqlExec) execObj(obj gqlObject, sels []gqlSelection) map[string]any {
	out := map[string]any{}
	for _, sel := range sels {
		switch sel.kind {
		case "field":
			v, err := obj.gqlField(e, sel.name, e.resolveArgs(sel.args))
			if err != nil {
				e.errs = append(e.errs, gqlError{Message: err.Error()})
				out[sel.outName()] = nil
				continue
			}
			out[sel.outName()] = e.execValue(v, sel.sels)
		case "spread":
			frag, ok := e.frags[sel.fragName]
			if !ok {
				continue
			}
			if frag.typeCond == "" || frag.typeCond == obj.gqlType() {
				for k, v := range e.execObj(obj, frag.sels) {
					out[k] = v
				}
			}
		case "inline":
			if sel.typeCond == "" || sel.typeCond == obj.gqlType() {
				for k, v := range e.execObj(obj, sel.sels) {
					out[k] = v
				}
			}
		}
	}
	return out
}

func (e *gqlExec) execValue(v any, sels []gqlSelection) any {
	switch t := v.(type) {
	case nil, string, int64, bool:
		return v
	case gqlObject:
		if len(sels) == 0 {
			return nil
		}
		return e.execObj(t, sels)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = e.execValue(item, sels)
		}
		return out
	case []gqlObject:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = e.execValue(item, sels)
		}
		return out
	}
	return v
}

// ---- object implementations ---------------------------------------------

// generic map-backed object (mutation payloads)
type gqlMap struct {
	typ string
	m   map[string]any
}

func (g gqlMap) gqlType() string { return g.typ }
func (g gqlMap) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if v, ok := g.m[name]; ok {
		return v, nil
	}
	return nil, gqlErrField(name, g.typ)
}

type gqlQuery struct{ s *server }

func (g gqlQuery) gqlType() string { return "Query" }
func (g gqlQuery) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "viewer":
		return gqlUser{}, nil
	case "repository":
		owner := argStr(args, "owner")
		repo := argStr(args, "name")
		r := g.s.store.getRepo(owner, repo)
		if r == nil {
			return nil, nil
		}
		return gqlRepo{r: r}, nil
	case "node":
		id := argStr(args, "id")
		return e.resolveNode(id), nil
	case "__type":
		return e.resolveType(argStr(args, "name")), nil
	}
	return nil, gqlErrField(name, "Query")
}

type gqlMutation struct{ s *server }

func (g gqlMutation) gqlType() string { return "Mutation" }
func (g gqlMutation) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch name {
	case "createIssue":
		r := e.repoByGlobalID(argStr(input, "repositoryId"))
		if r == nil {
			return nil, fmt.Errorf("Could not resolve to a Repository with the id of %q.", argStr(input, "repositoryId"))
		}
		iss := r.newIssue(argStr(input, "title"), argStr(input, "body"), nil)
		return gqlMap{"CreateIssuePayload", map[string]any{"issue": gqlIssue{r: r, iss: iss}}}, nil
	case "closeIssue":
		iss, r := e.issueByGlobalID(argStr(input, "issueId"))
		if iss == nil {
			return nil, fmt.Errorf("Could not resolve to an Issue with the id of %q.", argStr(input, "issueId"))
		}
		iss.State = "closed"
		iss.invalidate()
		return gqlMap{"CloseIssuePayload", map[string]any{"issue": gqlIssue{r: r, iss: iss}}}, nil
	case "createPullRequest":
		r := e.repoByGlobalID(argStr(input, "repositoryId"))
		if r == nil {
			return nil, fmt.Errorf("Could not resolve to a Repository with the id of %q.", argStr(input, "repositoryId"))
		}
		p, errMsg := r.newPull(argStr(input, "title"), "", argStr(input, "headRefName"), argStr(input, "baseRefName"))
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return gqlMap{"CreatePullRequestPayload", map[string]any{"pullRequest": gqlPull{r: r, p: p}}}, nil
	case "mergePullRequest":
		p, r := e.pullByGlobalID(argStr(input, "pullRequestId"))
		if p == nil {
			return nil, fmt.Errorf("Could not resolve to a PullRequest with the id of %q.", argStr(input, "pullRequestId"))
		}
		if _, reason := r.doMerge(p); reason == "conflict" {
			return nil, fmt.Errorf("Merge conflict")
		}
		return gqlMap{"MergePullRequestPayload", map[string]any{"pullRequest": gqlPull{r: r, p: p}}}, nil
	case "addComment":
		iss, r := e.issueByGlobalID(argStr(input, "subjectId"))
		if iss == nil {
			return nil, fmt.Errorf("Could not resolve to an Issue with the id of %q.", argStr(input, "subjectId"))
		}
		c := r.addComment(iss, argStr(input, "body"))
		return gqlMap{"AddCommentPayload", map[string]any{"commentEdge": gqlCommentEdge{r: r, c: c}}}, nil
	}
	return nil, gqlErrField(name, "Mutation")
}

type gqlUser struct{}

func (g gqlUser) gqlType() string { return "User" }
func (g gqlUser) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "login" {
		return viewerLogin, nil
	}
	return nil, gqlErrField(name, "User")
}

type gqlRepo struct{ r *Repo }

func (g gqlRepo) gqlType() string { return "Repository" }
func (g gqlRepo) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	r := g.r
	switch name {
	case "id":
		return repoGlobalID(r), nil
	case "databaseId":
		return int64(r.ID), nil
	case "name":
		return r.Name, nil
	case "nameWithOwner":
		return r.Owner + "/" + r.Name, nil
	case "owner":
		return gqlUser{}, nil
	case "defaultBranchRef":
		if r.defaultBranchOID() == "" {
			return nil, nil
		}
		return gqlBranchRef{name: r.DefaultBranch}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		iss := r.Issues[argInt(args, "number", 0)]
		if iss == nil {
			return nil, nil
		}
		return gqlIssue{r: r, iss: iss}, nil
	case "issues":
		return e.issueConnection(r, args), nil
	case "pullRequests":
		return e.pullConnection(r, args), nil
	case "pullRequest":
		p := r.Pulls[argInt(args, "number", 0)]
		if p == nil {
			return nil, nil
		}
		return gqlPull{r: r, p: p}, nil
	}
	return nil, gqlErrField(name, "Repository")
}

type gqlBranchRef struct{ name string }

func (g gqlBranchRef) gqlType() string { return "Ref" }
func (g gqlBranchRef) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "name" {
		return g.name, nil
	}
	return nil, gqlErrField(name, "Ref")
}

type gqlIssue struct {
	r   *Repo
	iss *Issue
}

func (g gqlIssue) gqlType() string { return "Issue" }
func (g gqlIssue) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return issueGlobalID(g.r, g.iss.Number), nil
	case "number":
		return int64(g.iss.Number), nil
	case "title":
		return g.iss.Title, nil
	case "body":
		return g.iss.Body, nil
	case "state":
		if g.iss.State == "open" {
			return "OPEN", nil
		}
		return "CLOSED", nil
	}
	return nil, gqlErrField(name, "Issue")
}

type gqlPull struct {
	r *Repo
	p *Pull
}

func (g gqlPull) gqlType() string { return "PullRequest" }
func (g gqlPull) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return pullGlobalID(g.r, g.p.Number), nil
	case "number":
		return int64(g.p.Number), nil
	case "title":
		return g.p.Title, nil
	case "headRefName":
		return g.p.Head, nil
	case "baseRefName":
		return g.p.Base, nil
	case "mergeable":
		if g.p.Merged || g.p.State != "open" {
			return "UNKNOWN", nil
		}
		if g.r.prConflict(g.p) {
			return "CONFLICTING", nil
		}
		return "MERGEABLE", nil
	case "state":
		if g.p.Merged {
			return "MERGED", nil
		}
		if g.p.State == "open" {
			return "OPEN", nil
		}
		return "CLOSED", nil
	case "merged":
		return g.p.Merged, nil
	case "files":
		first := argInt(args, "first", 100)
		files := g.p.Files
		if first < len(files) {
			files = files[:first]
		}
		return gqlFilesConn{files: files}, nil
	}
	return nil, gqlErrField(name, "PullRequest")
}

type gqlFilesConn struct{ files []PRFile }

func (g gqlFilesConn) gqlType() string { return "PullRequestChangedFileConnection" }
func (g gqlFilesConn) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "nodes" {
		out := make([]any, len(g.files))
		for i, f := range g.files {
			out[i] = gqlFile{path: f.Filename}
		}
		return out, nil
	}
	return nil, gqlErrField(name, "PullRequestChangedFileConnection")
}

type gqlFile struct{ path string }

func (g gqlFile) gqlType() string { return "PullRequestChangedFile" }
func (g gqlFile) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "path" {
		return g.path, nil
	}
	return nil, gqlErrField(name, "PullRequestChangedFile")
}

type gqlIssueConn struct {
	items    []*Issue
	r        *Repo
	hasNext  bool
	endCur   string
}

func (g gqlIssueConn) gqlType() string { return "IssueConnection" }
func (g gqlIssueConn) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "nodes":
		out := make([]any, len(g.items))
		for i, iss := range g.items {
			out[i] = gqlIssue{r: g.r, iss: iss}
		}
		return out, nil
	case "pageInfo":
		return gqlPageInfo{hasNext: g.hasNext, endCursor: g.endCur}, nil
	case "totalCount":
		return int64(len(g.items)), nil
	}
	return nil, gqlErrField(name, "IssueConnection")
}

type gqlPullConn struct {
	items []*Pull
	r     *Repo
}

func (g gqlPullConn) gqlType() string { return "PullRequestConnection" }
func (g gqlPullConn) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "nodes" {
		out := make([]any, len(g.items))
		for i, p := range g.items {
			out[i] = gqlPull{r: g.r, p: p}
		}
		return out, nil
	}
	if name == "totalCount" {
		return int64(len(g.items)), nil
	}
	return nil, gqlErrField(name, "PullRequestConnection")
}

type gqlPageInfo struct {
	hasNext   bool
	endCursor string
}

func (g gqlPageInfo) gqlType() string { return "PageInfo" }
func (g gqlPageInfo) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "hasNextPage":
		return g.hasNext, nil
	case "endCursor":
		return g.endCursor, nil
	case "hasPreviousPage":
		return false, nil
	case "startCursor":
		return g.endCursor, nil
	}
	return nil, gqlErrField(name, "PageInfo")
}

type gqlTypeObj struct{ name string }

func (g gqlTypeObj) gqlType() string { return "__Type" }
func (g gqlTypeObj) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "name":
		return g.name, nil
	case "fields":
		fields := typeFields(g.name)
		out := make([]any, len(fields))
		for i, f := range fields {
			out[i] = gqlNamed{name: f}
		}
		return out, nil
	case "enumValues":
		vals := typeEnumValues(g.name)
		out := make([]any, len(vals))
		for i, v := range vals {
			out[i] = gqlNamed{name: v}
		}
		return out, nil
	}
	return nil, gqlErrField(name, "__Type")
}

type gqlNamed struct{ name string }

func (g gqlNamed) gqlType() string { return "__Field" }
func (g gqlNamed) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "name" {
		return g.name, nil
	}
	return nil, gqlErrField(name, "__Field")
}

type gqlCommentEdge struct {
	r *Repo
	c *Comment
}

func (g gqlCommentEdge) gqlType() string { return "IssueCommentEdge" }
func (g gqlCommentEdge) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	if name == "node" {
		return gqlComment{r: g.r, c: g.c}, nil
	}
	return nil, gqlErrField(name, "IssueCommentEdge")
}

type gqlComment struct {
	r *Repo
	c *Comment
}

func (g gqlComment) gqlType() string { return "IssueComment" }
func (g gqlComment) gqlField(e *gqlExec, name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return commentGlobalID(g.r, g.c.ID), nil
	case "body":
		return g.c.Body, nil
	}
	return nil, gqlErrField(name, "IssueComment")
}

// ---- introspection tables -----------------------------------------------

func typeFields(name string) []string {
	switch name {
	case "Issue":
		return []string{"id", "number", "title", "body", "state", "labels", "comments", "author", "createdAt", "closedAt", "url", "repository"}
	case "PullRequest":
		return []string{"id", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "author", "createdAt", "mergedAt", "mergeCommit", "url", "repository"}
	case "Repository":
		return []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "createdAt", "updatedAt", "url"}
	}
	return nil
}

func typeEnumValues(name string) []string {
	if name == "SearchType" {
		return []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT", "MILESTONE", "WIKI", "PACKAGE"}
	}
	return nil
}

// ---- connections ----------------------------------------------------------

func (e *gqlExec) issueConnection(r *Repo, args map[string]any) gqlIssueConn {
	states := argStrList(args, "states")
	var nums []int
	for _, n := range r.IssueOrder {
		iss := r.Issues[n]
		if len(states) > 0 {
			want := false
			for _, st := range states {
				if (st == "OPEN" && iss.State == "open") || (st == "CLOSED" && iss.State == "closed") {
					want = true
				}
			}
			if !want {
				continue
			}
		}
		nums = append(nums, n)
	}
	// orderBy CREATED_AT DESC = descending number; default DESC.
	desc := true
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if d, ok := ob["direction"].(string); ok && d == "ASC" {
			desc = false
		}
	}
	if desc {
		for i, j := 0, len(nums)-1; i < j; i, j = i+1, j-1 {
			nums[i], nums[j] = nums[j], nums[i]
		}
	}
	// cursor: start after the issue whose number the cursor names.
	start := 0
	if cur := argStr(args, "after"); cur != "" {
		if n, ok := decodeCursor(cur); ok {
			for i, num := range nums {
				if num == n {
					start = i + 1
					break
				}
			}
		}
	}
	first := argInt(args, "first", 30)
	if first < 0 {
		first = 0
	}
	end := start + first
	if end > len(nums) {
		end = len(nums)
	}
	var items []*Issue
	for _, n := range nums[start:end] {
		items = append(items, r.Issues[n])
	}
	conn := gqlIssueConn{items: items, r: r, hasNext: end < len(nums)}
	if len(items) > 0 {
		conn.endCur = encodeCursor(items[len(items)-1].Number)
	}
	return conn
}

func (e *gqlExec) pullConnection(r *Repo, args map[string]any) gqlPullConn {
	first := argInt(args, "first", 30)
	if first < 0 {
		first = 0
	}
	var items []*Pull
	for _, n := range r.PullOrder {
		if len(items) >= first {
			break
		}
		items = append(items, r.Pulls[n])
	}
	return gqlPullConn{items: items, r: r}
}

func encodeCursor(n int) string {
	return b64opaque("cursor:" + strconv.Itoa(n))
}

func decodeCursor(cur string) (int, bool) {
	b, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return 0, false
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---- global id resolution -------------------------------------------------

func decodeGlobalID(id string) (kind string, repoID, num int, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", 0, 0, false
	}
	parts := strings.Split(string(b), ":")
	if len(parts) < 2 {
		return "", 0, 0, false
	}
	repoID, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, 0, false
	}
	if len(parts) == 3 {
		num, err = strconv.Atoi(parts[2])
		if err != nil {
			return "", 0, 0, false
		}
	}
	return parts[0], repoID, num, true
}

func (e *gqlExec) repoByID(id int) *Repo {
	for _, r := range e.s.store.repoList {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (e *gqlExec) repoByGlobalID(id string) *Repo {
	kind, repoID, _, ok := decodeGlobalID(id)
	if !ok || kind != "Repository" {
		return nil
	}
	return e.repoByID(repoID)
}

func (e *gqlExec) issueByGlobalID(id string) (*Issue, *Repo) {
	kind, repoID, num, ok := decodeGlobalID(id)
	if !ok || kind != "Issue" {
		return nil, nil
	}
	r := e.repoByID(repoID)
	if r == nil {
		return nil, nil
	}
	return r.Issues[num], r
}

func (e *gqlExec) pullByGlobalID(id string) (*Pull, *Repo) {
	kind, repoID, num, ok := decodeGlobalID(id)
	if !ok || kind != "PullRequest" {
		return nil, nil
	}
	r := e.repoByID(repoID)
	if r == nil {
		return nil, nil
	}
	return r.Pulls[num], r
}

func (e *gqlExec) resolveNode(id string) any {
	kind, repoID, num, ok := decodeGlobalID(id)
	if !ok {
		return nil
	}
	r := e.repoByID(repoID)
	if r == nil {
		return nil
	}
	switch kind {
	case "Repository":
		return gqlRepo{r: r}
	case "Issue":
		if iss := r.Issues[num]; iss != nil {
			return gqlIssue{r: r, iss: iss}
		}
	case "PullRequest":
		if p := r.Pulls[num]; p != nil {
			return gqlPull{r: r, p: p}
		}
	}
	return nil
}

func (e *gqlExec) resolveType(name string) any {
	switch name {
	case "Issue", "PullRequest", "Repository", "SearchType":
		return gqlTypeObj{name: name}
	}
	return nil
}

// ---- handler --------------------------------------------------------------

// routeGraphQL handles POST /api/graphql (single operation or a JSON
// array batch).
func (s *server) routeGraphQL(req *request) []byte {
	if !s.authorized(req) {
		return errResp(401, "Requires authentication")
	}
	if req.method != "POST" {
		return errResp(405, "Method Not Allowed")
	}
	body := req.body
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return errResp(400, "empty GraphQL body")
	}
	if strings.HasPrefix(trimmed, "[") {
		var ops []json.RawMessage
		if err := json.Unmarshal(body, &ops); err != nil {
			return errResp(400, "invalid JSON")
		}
		var results []any
		for _, raw := range ops {
			results = append(results, s.execGraphQL(raw))
		}
		out, _ := json.Marshal(results)
		return jsonResp(200, out)
	}
	out, _ := json.Marshal(s.execGraphQL(body))
	return jsonResp(200, out)
}

func (s *server) execGraphQL(body []byte) any {
	var op struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "invalid JSON: " + err.Error()}}}
	}
	doc, err := parseGraphQL(op.Query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "syntax error: " + err.Error()}}}
	}
	if len(doc.ops) == 0 {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "no operation"}}}
	}
	operation := doc.ops[0]
	ex := &gqlExec{s: s, vars: op.Variables, frags: doc.frags}
	if ex.vars == nil {
		ex.vars = map[string]any{}
	}
	// variable defaults
	for name, def := range operation.vars {
		if _, ok := ex.vars[name]; !ok {
			ex.vars[name] = ex.resolveValue(def)
		}
	}
	var root gqlObject
	if operation.kind == "mutation" {
		root = gqlMutation{s: s}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		data := ex.execObj(root, operation.sels)
		return gqlResult(data, ex.errs)
	}
	root = gqlQuery{s: s}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	data := ex.execObj(root, operation.sels)
	return gqlResult(data, ex.errs)
}

func gqlResult(data map[string]any, errs []gqlError) map[string]any {
	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return out
}
