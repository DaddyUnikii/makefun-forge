package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- global ids

func gqlEncode(payload string) string {
	return base64.StdEncoding.EncodeToString([]byte(payload))
}

func gqlDecode(id string) (string, bool) {
	b, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func repoGID(id int64) string             { return gqlEncode(fmt.Sprintf("Repository:%d", id)) }
func issueGID(repoID int64, n int) string { return gqlEncode(fmt.Sprintf("Issue:%d:%d", repoID, n)) }
func pullGID(repoID int64, n int) string {
	return gqlEncode(fmt.Sprintf("PullRequest:%d:%d", repoID, n))
}
func commentGID(id int64) string { return gqlEncode(fmt.Sprintf("Comment:%d", id)) }

// ---------------------------------------------------------------- AST

type gqlField struct {
	alias     string
	name      string
	args      map[string]interface{}
	subs      []gqlField
	conds     []string // inline fragment type conditions (merged with subs)
	fragSubs  [][]gqlField
	fragConds []string
}

type gqlOp struct {
	opType string // query | mutation
	name   string
	vars   map[string]gqlVar
	fields []gqlField
}

type gqlVar struct {
	goType string
	defVal interface{}
}

// ---------------------------------------------------------------- lexer

type gqlTok struct {
	kind string // name, string, int, float, punct
	val  string
	num  int64
}

func gqlLex(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{kind: "punct", val: "..."})
			i += 3
		case strings.ContainsRune("{}()[]:!=$@&|", rune(c)):
			toks = append(toks, gqlTok{kind: "punct", val: string(c)})
			i++
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < n {
				if src[j] == '\\' && j+1 < n {
					switch src[j+1] {
					case 'n':
						sb.WriteByte('\n')
						j += 2
					case 't':
						sb.WriteByte('\t')
						j += 2
					case '"':
						sb.WriteByte('"')
						j += 2
					case '\\':
						sb.WriteByte('\\')
						j += 2
					case '/':
						sb.WriteByte('/')
						j += 2
					default:
						sb.WriteByte(src[j+1])
						j += 2
					}
					continue
				}
				if src[j] == '"' {
					break
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{kind: "string", val: sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '-' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			neg := false
			if src[j] == '-' {
				neg = true
				j++
			}
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			isFloat := false
			if j < n && src[j] == '.' && j+1 < n && src[j+1] >= '0' && src[j+1] <= '9' {
				isFloat = true
				j++
				for j < n && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			numStr := src[i:j]
			if neg {
				numStr = "-" + src[i+1:j]
			}
			if isFloat {
				f, _ := strconv.ParseFloat(numStr, 64)
				toks = append(toks, gqlTok{kind: "float", val: numStr, num: int64(f * 1000000)})
			} else {
				v, _ := strconv.ParseInt(numStr, 10, 64)
				toks = append(toks, gqlTok{kind: "int", val: numStr, num: v})
			}
			i = j
		default:
			// name: [A-Za-z_][A-Za-z0-9_]*
			j := i
			for j < n && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || (j > i && src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected character %q", string(c))
			}
			toks = append(toks, gqlTok{kind: "name", val: src[i:j]})
			i = j
		}
	}
	toks = append(toks, gqlTok{kind: "eof"})
	return toks, nil
}

// ---------------------------------------------------------------- parser

type gqlParser struct {
	toks    []gqlTok
	pos     int
	opVars  *map[string]gqlVar
	runtime map[string]interface{}
}

func (p *gqlParser) peek() gqlTok { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(v string) error {
	if t := p.next(); t.kind != "punct" || t.val != v {
		return fmt.Errorf("expected %q", v)
	}
	return nil
}
func (p *gqlParser) expectName() (string, error) {
	t := p.next()
	if t.kind != "name" {
		return "", fmt.Errorf("expected a name")
	}
	return t.val, nil
}

// parseValue parses a literal or variable reference.
func (p *gqlParser) parseValue(vars map[string]gqlVar) (interface{}, error) {
	t := p.peek()
	switch t.kind {
	case "string":
		p.next()
		return t.val, nil
	case "int":
		p.next()
		return float64(t.num), nil
	case "float":
		p.next()
		return float64(t.num) / 1000000.0, nil
	case "name":
		if t.val == "true" {
			p.next()
			return true, nil
		}
		if t.val == "false" {
			p.next()
			return false, nil
		}
		if t.val == "null" {
			p.next()
			return nil, nil
		}
		p.next()
		return gqlEnumValue(t.val), nil
	case "punct":
		switch t.val {
		case "$":
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if p.runtime != nil {
				if v, ok := p.runtime[name]; ok {
					return v, nil
				}
			}
			if p.opVars != nil {
				if v, ok := (*p.opVars)[name]; ok {
					return v.defVal, nil
				}
			}
			return nil, nil
		case "[":
			p.next()
			var arr []interface{}
			for p.peek().kind != "punct" || p.peek().val != "]" {
				v, err := p.parseValue(vars)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			p.next()
			return arr, nil
		case "{":
			p.next()
			obj := map[string]interface{}{}
			for !(p.peek().kind == "punct" && p.peek().val == "}") {
				key, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue(vars)
				if err != nil {
					return nil, err
				}
				obj[key] = v
			}
			p.next()
			return obj, nil
		}
	}
	return nil, fmt.Errorf("unexpected token %q in value", t.val)
}

func (p *gqlParser) parseField(vars map[string]gqlVar) (gqlField, error) {
	var f gqlField
	nameOrAlias, err := p.expectName()
	if err != nil {
		return f, err
	}
	f.name = nameOrAlias
	f.alias = nameOrAlias
	// alias?
	if p.peek().kind == "punct" && p.peek().val == ":" {
		p.next()
		realName, err := p.expectName()
		if err != nil {
			return f, err
		}
		f.name = realName
	}
	// arguments
	if p.peek().kind == "punct" && p.peek().val == "(" {
		p.next()
		f.args = map[string]interface{}{}
		for !(p.peek().kind == "punct" && p.peek().val == ")") {
			key, err := p.expectName()
			if err != nil {
				return f, err
			}
			if err := p.expect(":"); err != nil {
				return f, err
			}
			v, err := p.parseValue(vars)
			if err != nil {
				return f, err
			}
			f.args[key] = v
		}
		p.next()
	}
	// selection set / directives
	for {
		if p.peek().kind == "punct" && p.peek().val == "@" {
			p.next()
			p.expectName()
			if p.peek().kind == "punct" && p.peek().val == "(" {
				p.next()
				depth := 1
				for depth > 0 {
					t := p.next()
					if t.kind == "punct" && t.val == "(" {
						depth++
					}
					if t.kind == "punct" && t.val == ")" {
						depth--
					}
					if t.kind == "eof" {
						return f, fmt.Errorf("unterminated directive")
					}
				}
			}
			continue
		}
		break
	}
	if p.peek().kind == "punct" && p.peek().val == "{" {
		p.next()
		for !(p.peek().kind == "punct" && p.peek().val == "}") {
			if p.peek().kind == "punct" && p.peek().val == "..." {
				p.next()
				on, err := p.expectName() // "on"
				if err != nil || on != "on" {
					return f, fmt.Errorf("expected `on` in inline fragment")
				}
				typeName, err := p.expectName()
				if err != nil {
					return f, err
				}
				if err := p.expect("{"); err != nil {
					return f, err
				}
				var subs []gqlField
				for !(p.peek().kind == "punct" && p.peek().val == "}") {
					sub, err := p.parseField(vars)
					if err != nil {
						return f, err
					}
					subs = append(subs, sub)
				}
				p.next()
				f.conds = append(f.conds, typeName)
				f.fragSubs = append(f.fragSubs, subs)
			} else {
				sub, err := p.parseField(vars)
				if err != nil {
					return f, err
				}
				f.subs = append(f.subs, sub)
			}
		}
		p.next()
	}
	return f, nil
}

// parseDocument parses one operation document.
func parseGQLDoc(src string, runtime map[string]interface{}) (gqlOp, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return gqlOp{}, err
	}
	p := &gqlParser{toks: toks, runtime: runtime}
	var op gqlOp
	op.vars = map[string]gqlVar{}
	p.opVars = &op.vars
	for {
		t := p.peek()
		if t.kind == "eof" {
			break
		}
		if t.kind == "punct" && t.val == "{" {
			// anonymous query
			p.next()
			for !(p.peek().kind == "punct" && p.peek().val == "}") {
				f, err := p.parseField(op.vars)
				if err != nil {
					return op, err
				}
				op.fields = append(op.fields, f)
			}
			p.next()
			op.opType = "query"
			continue
		}
		if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
			op.opType = t.val
			p.next()
			if p.peek().kind == "name" {
				op.name = p.next().val
			}
			// variable definitions
			if p.peek().kind == "punct" && p.peek().val == "(" {
				p.next()
				for !(p.peek().kind == "punct" && p.peek().val == ")") {
					if p.next().kind != "punct" || p.toks[p.pos-1].val != "$" {
						return op, fmt.Errorf("expected $ in variable definition")
					}
					vname, err := p.expectName()
					if err != nil {
						return op, err
					}
					if err := p.expect(":"); err != nil {
						return op, err
					}
					// type: Name[!] or [Name!]!
					var types []string
					for {
						tt := p.peek()
						if tt.kind == "punct" && tt.val == "[" {
							p.next()
							types = append(types, "[")
							continue
						}
						if tt.kind == "punct" && tt.val == "]" {
							p.next()
							types = append(types, "]")
							continue
						}
						if tt.kind == "punct" && tt.val == "!" {
							p.next()
							types = append(types, "!")
							continue
						}
						if tt.kind == "name" {
							p.next()
							types = append(types, tt.val)
							continue
						}
						break
					}
					gv := gqlVar{goType: strings.Join(types, "")}
					// default value
					if p.peek().kind == "punct" && p.peek().val == "=" {
						p.next()
						dv, err := p.parseValue(op.vars)
						if err != nil {
							return op, err
						}
						gv.defVal = dv
					}
					op.vars[vname] = gv
				}
				p.next()
			}
			// directives on the operation (skip)
			for p.peek().kind == "punct" && p.peek().val == "@" {
				p.next()
				p.expectName()
				if p.peek().kind == "punct" && p.peek().val == "(" {
					p.next()
					depth := 1
					for depth > 0 {
						tt := p.next()
						if tt.kind == "punct" && tt.val == "(" {
							depth++
						}
						if tt.kind == "punct" && tt.val == ")" {
							depth--
						}
					}
				}
			}
			// selection set
			if err := p.expect("{"); err != nil {
				return op, err
			}
			for !(p.peek().kind == "punct" && p.peek().val == "}") {
				f, err := p.parseField(op.vars)
				if err != nil {
					return op, err
				}
				op.fields = append(op.fields, f)
			}
			p.next()
			continue
		}
		if t.kind == "name" && t.val == "fragment" {
			// skip fragment definition
			p.next()
			p.expectName() // fragment name
			on, _ := p.expectName()
			_ = on
			p.expectName() // type condition
			if err := p.expect("{"); err != nil {
				return op, err
			}
			depth := 1
			for depth > 0 {
				tt := p.next()
				if tt.kind == "punct" && tt.val == "{" {
					depth++
				}
				if tt.kind == "punct" && tt.val == "}" {
					depth--
				}
				if tt.kind == "eof" {
					return op, fmt.Errorf("unterminated fragment")
				}
			}
			continue
		}
		return op, fmt.Errorf("unexpected token %q", t.val)
	}
	return op, nil
}

// gqlEnumValue marks an unquoted enum/name literal in argument position.
type gqlEnumValue string

// ---------------------------------------------------------------- object wrappers

type gqlRepos struct{ r *Repo }
type gqlIssue2 struct {
	repo *Repo
	iss  *Issue
}
type gqlPull2 struct {
	repo *Repo
	p    *Pull
}
type gqlUser struct{ login string }
type gqlComment struct {
	id   string
	body string
}
type gqlRef struct{ name string }
type gqlFile struct{ path string }
type gqlPullFilesConn struct{ nodes []interface{} }
type gqlPageInfo struct {
	hasNext bool
	endCur  string
}
type gqlIssuesConn struct {
	repo    *Repo
	nodes   []*Issue
	hasNext bool
	endCur  string
}
type gqlPullsConn struct {
	repo  *Repo
	nodes []*Pull
}
type gqlTypeObj struct{ name string }
type gqlTypeField struct{ name string }

// ---------------------------------------------------------------- execution

// coreerces an arg value into a string (enum or string).
func argStr(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case gqlEnumValue:
		return string(t), true
	}
	return "", false
}

func argInt(v interface{}) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	}
	return 0, false
}

func argObj(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

// matchTypeName returns true when the given GraphQL type name matches a wrapper.
func matchType(v interface{}, name string) bool {
	switch v.(type) {
	case *gqlRepos:
		return name == "Repository"
	case *gqlIssue2:
		return name == "Issue"
	case *gqlPull2:
		return name == "PullRequest"
	case *gqlUser:
		return name == "User"
	case *gqlComment:
		return name == "Comment"
	case gqlIssuesConn:
		return name == "IssueConnection"
	case gqlPullsConn:
		return name == "PullRequestConnection"
	}
	return false
}

// resolveField resolves one field on a value; returns (value, valid, explicitNull).
func resolveField(v interface{}, name string, args map[string]interface{}) (interface{}, bool) {
	if m, okM := v.(map[string]interface{}); okM {
		if val, exists := m[name]; exists {
			return val, true
		}
		return nil, false
	}
	switch node := v.(type) {
	case *gqlRepos:
		r := node.r
		switch name {
		case "id":
			return repoGID(r.ID), true
		case "databaseId":
			return float64(r.ID), true
		case "name":
			return r.Name, true
		case "nameWithOwner":
			return r.Owner + "/" + r.Name, true
		case "owner":
			return &gqlUser{login: VIEWER_LOGIN}, true
		case "defaultBranchRef":
			return &gqlRef{name: DEFAULT_BRANCH}, true
		case "viewerPermission":
			return gqlEnumValue("ADMIN"), true
		case "hasIssuesEnabled":
			return true, true
		case "issue":
			n, _ := argInt(args["number"])
			if iss := getIssueSafe(r.ID, n); iss != nil {
				return &gqlIssue2{repo: r, iss: iss}, true
			}
			return nil, true
		case "issues":
			return resolveIssuesConn(r, args), true
		case "pullRequests":
			return resolvePullsConn(r, args), true
		case "pullRequest":
			n, _ := argInt(args["number"])
			if p := getPullSafe(r.ID, n); p != nil {
				return &gqlPull2{repo: r, p: p}, true
			}
			return nil, true
		}
	case *gqlIssue2:
		i := node.iss
		switch name {
		case "id":
			return issueGID(node.repo.ID, i.Number), true
		case "number":
			return float64(i.Number), true
		case "title":
			return i.Title, true
		case "state":
			if i.State == "closed" {
				return gqlEnumValue("CLOSED"), true
			}
			return gqlEnumValue("OPEN"), true
		case "body":
			return i.Body, true
		}
	case *gqlPull2:
		p := node.p
		switch name {
		case "id":
			return pullGID(node.repo.ID, p.Number), true
		case "number":
			return float64(p.Number), true
		case "state":
			switch p.State {
			case "open":
				return gqlEnumValue("OPEN"), true
			case "merged":
				return gqlEnumValue("MERGED"), true
			default:
				return gqlEnumValue("CLOSED"), true
			}
		case "merged":
			return p.Merged, true
		case "headRefName":
			return p.HeadRef, true
		case "baseRefName":
			return p.BaseRef, true
		case "mergeable":
			if p.Merged {
				return gqlEnumValue("MERGED"), true
			}
			if storePullMergeable(node.repo, p) {
				return gqlEnumValue("MERGEABLE"), true
			}
			return gqlEnumValue("CONFLICTING"), true
		case "files":
			return resolvePullFiles(node.repo, p), true
		}
	case *gqlUser:
		if name == "login" {
			return node.login, true
		}
	case *gqlComment:
		switch name {
		case "id":
			return node.id, true
		case "body":
			return node.body, true
		}
	case *gqlRef:
		if name == "name" {
			return node.name, true
		}
	case *gqlFile:
		if name == "path" {
			return node.path, true
		}
	case *gqlPullFilesConn:
		if name == "nodes" {
			return node.nodes, true
		}
	case gqlIssuesConn:
		switch name {
		case "nodes":
			out := make([]interface{}, 0, len(node.nodes))
			for _, i := range node.nodes {
				out = append(out, &gqlIssue2{repo: node.repo, iss: i})
			}
			return out, true
		case "pageInfo":
			return gqlPageInfo{hasNext: node.hasNext, endCur: node.endCur}, true
		}
	case gqlPullsConn:
		if name == "nodes" {
			out := make([]interface{}, 0, len(node.nodes))
			for _, p := range node.nodes {
				out = append(out, &gqlPull2{repo: node.repo, p: p})
			}
			return out, true
		}
	case gqlPageInfo:
		switch name {
		case "hasNextPage":
			return node.hasNext, true
		case "endCursor":
			if node.endCur == "" {
				return nil, true
			}
			return node.endCur, true
		}
	case gqlTypeObj:
		switch name {
		case "fields":
			return resolveTypeFields(node.name), true
		case "enumValues":
			return resolveTypeEnums(node.name), true
		}
	}
	return nil, false
}

func getIssueSafe(repoID int64, n int) *Issue {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.getIssue(repoID, n)
}

func getPullSafe(repoID int64, n int) *Pull {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.getPull(repoID, n)
}

var issueFieldNames = []string{"id", "number", "title", "state", "body", "labels", "createdAt", "updatedAt", "closed", "comments"}
var pullFieldNames = []string{"id", "number", "state", "title", "body", "headRefName", "baseRefName", "mergeable", "merged", "files", "headRefOid", "baseRefOid"}
var repoFieldNames = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate"}

func resolveTypeFields(typeName string) []interface{} {
	var names []string
	switch typeName {
	case "Issue":
		names = issueFieldNames
	case "PullRequest":
		names = pullFieldNames
	case "Repository":
		names = repoFieldNames
	default:
		names = []string{"id"}
	}
	out := make([]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]interface{}{"name": n})
	}
	return out
}

var searchTypeEnums = []string{"ISSUE", "REPOSITORY", "USER", "CODE", "COMMIT", "DISCUSSION"}

func resolveTypeEnums(typeName string) []interface{} {
	var names []string
	switch typeName {
	case "SearchType", "IssueState", "MergeableState", "PullRequestState", "PullRequestMergeMethod", "RepositoryPermission", "IssueOrderField", "OrderDirection":
		names = enumNames(typeName)
	default:
		names = []string{}
	}
	out := make([]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]interface{}{"name": n})
	}
	return out
}

func enumNames(typeName string) []string {
	switch typeName {
	case "SearchType":
		return searchTypeEnums
	case "IssueState":
		return []string{"OPEN", "CLOSED"}
	case "MergeableState":
		return []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}
	case "PullRequestState":
		return []string{"OPEN", "CLOSED", "MERGED"}
	case "PullRequestMergeMethod":
		return []string{"MERGE", "SQUASH", "REBASE"}
	case "RepositoryPermission":
		return []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}
	case "IssueOrderField":
		return []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}
	case "OrderDirection":
		return []string{"ASC", "DESC"}
	}
	return nil
}

// resolveIssuesConn builds the filtered/ordered issues connection.
func resolveIssuesConn(r *Repo, args map[string]interface{}) interface{} {
	first := 100
	if v, ok := argInt(args["first"]); ok {
		first = v
	}
	states := map[gqlEnumValue]bool{}
	if raw, ok := args["states"]; ok {
		if arr, ok := raw.([]interface{}); ok {
			for _, e := range arr {
				if s, ok := e.(gqlEnumValue); ok {
					states[s] = true
				}
			}
		}
	}
	// orderBy (default CREATED_AT DESC)
	desc := true
	if ob, ok := argObj(args["orderBy"]); ok {
		if dir, ok := argStr(ob["direction"]); ok && dir == "ASC" {
			desc = false
		}
	}
	hasAfter := false
	after := int64(0)
	if v, ok := args["after"]; ok && v != nil {
		if s, ok := argStr(v); ok && s != "" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				after = n
				hasAfter = true
			}
		}
	}

	store.mu.RLock()
	var all []*Issue
	for _, i := range store.issues[r.ID] {
		if len(states) > 0 {
			st := gqlEnumValue("OPEN")
			if i.State == "closed" {
				st = gqlEnumValue("CLOSED")
			}
			if !states[st] {
				continue
			}
		}
		if hasAfter {
			if desc {
				if int64(i.Number) >= after {
					continue
				}
			} else {
				if int64(i.Number) <= after {
					continue
				}
			}
		}
		all = append(all, i)
	}
	store.mu.RUnlock()
	sort.SliceStable(all, func(a, b int) bool {
		if desc {
			return all[a].Number > all[b].Number
		}
		return all[a].Number < all[b].Number
	})
	nodes := all
	hasNext := false
	endCur := ""
	if len(nodes) > first {
		nodes = nodes[:first]
		hasNext = true
	}
	if len(nodes) > 0 {
		endCur = strconv.Itoa(nodes[len(nodes)-1].Number)
	}
	return gqlIssuesConn{repo: r, nodes: nodes, hasNext: hasNext, endCur: endCur}
}

func resolvePullsConn(r *Repo, args map[string]interface{}) interface{} {
	first := 100
	if v, ok := argInt(args["first"]); ok {
		first = v
	}
	store.mu.RLock()
	var nodes []*Pull
	for _, p := range store.pulls[r.ID] {
		nodes = append(nodes, p)
	}
	store.mu.RUnlock()
	sort.SliceStable(nodes, func(a, b int) bool { return nodes[a].Number < nodes[b].Number })
	if len(nodes) > first {
		nodes = nodes[:first]
	}
	return gqlPullsConn{repo: r, nodes: nodes}
}

// storePullMergeable mirrors the REST conflict check for the GraphQL field.
func storePullMergeable(repo *Repo, p *Pull) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	r := repo
	_ = r
	if p.Merged {
		return true
	}
	baseTip, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+p.BaseRef)
	if !ok {
		return false
	}
	baseTree, ok1 := store.commitTree(p.BaseTipAtOpen)
	headTree, ok2 := store.commitTree(p.HeadSha)
	curBaseTree, ok3 := store.commitTree(baseTip)
	if !ok1 || !ok2 || !ok3 {
		return false
	}
	baseBlobs := store.flattenTree(baseTree)
	headBlobs := store.flattenTree(headTree)
	curBlobs := store.flattenTree(curBaseTree)
	for path, h := range headBlobs {
		if b, inBase := baseBlobs[path]; inBase && b.oid == h.oid {
			continue
		}
		var mergeBaseOid string
		if b, inBase := baseBlobs[path]; inBase {
			mergeBaseOid = b.oid
		}
		curOid := ""
		if c, inCur := curBlobs[path]; inCur {
			curOid = c.oid
		}
		if mergeBaseOid != curOid {
			return false
		}
	}
	return true
}

func resolvePullFiles(repo *Repo, p *Pull) *gqlPullFilesConn {
	files := pullFiles(repo, p)
	out := make([]interface{}, 0, len(files))
	for _, f := range files {
		path, _ := f["filename"].(string)
		out = append(out, &gqlFile{path: path})
	}
	return &gqlPullFilesConn{nodes: out}
}

// projectionOf returns a field's sub-selections with its inline fragments
// flattened in as condition-carrying fields, so a projection applies both a
// field's direct subfields and its `... on Type { }` fragments.
func projectionOf(f gqlField) []gqlField {
	out := append([]gqlField{}, f.subs...)
	for i, cond := range f.conds {
		out = append(out, gqlField{conds: []string{cond}, fragSubs: [][]gqlField{f.fragSubs[i]}})
	}
	return out
}

// selectFields projects a value through a list of selections.
func selectFields(v interface{}, fields []gqlField, path []interface{}, errs *[]map[string]interface{}) interface{} {
	if len(fields) == 0 {
		return nil
	}
	result := map[string]interface{}{}
	for _, f := range fields {
		key := f.alias
		// inline fragments with conditions
		if len(f.conds) > 0 {
			for idx, cond := range f.conds {
				if v != nil && matchType(v, cond) {
					sub := selectFields(v, f.fragSubs[idx], path, errs)
					if m, ok := sub.(map[string]interface{}); ok {
						for k, val := range m {
							result[k] = val
						}
					}
				}
			}
			continue
		}
		val, ok := resolveField(v, f.name, f.args)
		if !ok {
			msg := fmt.Sprintf("Field '%s' doesn't exist on type '%s'", f.name, typeNameOf(v))
			*errs = append(*errs, map[string]interface{}{"message": msg})
			result[key] = nil
			continue
		}
		if proj := projectionOf(f); len(proj) > 0 {
			val = selectValue(val, proj, append(path, f.alias), errs)
		}
		result[key] = val
	}
	return result
}

func typeNameOf(v interface{}) string {
	switch v.(type) {
	case *gqlRepos:
		return "Repository"
	case *gqlIssue2:
		return "Issue"
	case *gqlPull2:
		return "PullRequest"
	case *gqlUser:
		return "User"
	case *gqlComment:
		return "Comment"
	case *gqlRef:
		return "Ref"
	case *gqlFile:
		return "PullRequestChangedFile"
	case *gqlPullFilesConn:
		return "PullRequestChangedFileConnection"
	case gqlIssuesConn:
		return "IssueConnection"
	case gqlPullsConn:
		return "PullRequestConnection"
	case gqlPageInfo:
		return "PageInfo"
	case gqlTypeObj:
		return "__Type"
	}
	return "Unknown"
}

// selectValue applies selections to a scalar/list/object value.
func selectValue(val interface{}, fields []gqlField, path []interface{}, errs *[]map[string]interface{}) interface{} {
	switch t := val.(type) {
	case nil:
		return nil
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, item := range t {
			out = append(out, selectValue(item, fields, path, errs))
		}
		return out
	case map[string]interface{}:
		return selectFields(t, fields, path, errs)
	default:
		return selectFields(val, fields, path, errs)
	}
}

// ---------------------------------------------------------------- mutation executors

func findRepoByGID(id string) *Repo {
	payload, ok := gqlDecode(id)
	if !ok {
		return nil
	}
	if !strings.HasPrefix(payload, "Repository:") {
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(payload, "Repository:"), 10, 64)
	if err != nil {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, r := range store.repos {
		if r.ID == n {
			return r
		}
	}
	return nil
}

func findIssueByGID(id string) (*Repo, *Issue) {
	payload, ok := gqlDecode(id)
	if !ok || !strings.HasPrefix(payload, "Issue:") {
		return nil, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(payload, "Issue:"), ":", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, r := range store.repos {
		if r.ID == repoID {
			return r, store.getIssue(repoID, num)
		}
	}
	return nil, nil
}

func findPullByGID(id string) (*Repo, *Pull) {
	payload, ok := gqlDecode(id)
	if !ok || !strings.HasPrefix(payload, "PullRequest:") {
		return nil, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(payload, "PullRequest:"), ":", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, r := range store.repos {
		if r.ID == repoID {
			return r, store.getPull(repoID, num)
		}
	}
	return nil, nil
}

type mutationResult struct {
	data map[string]interface{}
	ok   bool
	err  string
}

func execMutation(f *gqlField) mutationResult {
	switch f.name {
	case "createIssue":
		arg := firstArgOf(f)
		in, _ := argObj(arg["input"])
		repo := findRepoByGID(repoIDString(in["repositoryId"]))
		if repo == nil {
			return mutationResult{err: "repository not found"}
		}
		title, _ := argStr(in["title"])
		body, _ := argStr(in["body"])
		if title == "" {
			return mutationResult{err: "title is required"}
		}
		store.mu.Lock()
		iss := store.addIssue(repo.ID, title, body, nil)
		store.mu.Unlock()
		return mutationResult{data: map[string]interface{}{
			"issue": &gqlIssue2{repo: repo, iss: iss},
		}, ok: true}
	case "closeIssue":
		arg := firstArgOf(f)
		in, _ := argObj(arg["input"])
		repo, iss := findIssueByGID(repoIDString(in["issueId"]))
		if iss == nil {
			return mutationResult{err: "issue not found"}
		}
		store.mu.Lock()
		iss.State = "closed"
		store.mu.Unlock()
		return mutationResult{data: map[string]interface{}{
			"issue": &gqlIssue2{repo: repo, iss: iss},
		}, ok: true}
	case "createPullRequest":
		arg := firstArgOf(f)
		in, _ := argObj(arg["input"])
		repo := findRepoByGID(repoIDString(in["repositoryId"]))
		if repo == nil {
			return mutationResult{err: "repository not found"}
		}
		head, _ := argStr(in["headRefName"])
		base, _ := argStr(in["baseRefName"])
		title, _ := argStr(in["title"])
		body, _ := argStr(in["body"])
		store.mu.Lock()
		headSha, hok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+head)
		baseSha, bok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+base)
		var p *Pull
		if hok && bok {
			p = store.addPull(repo.ID, title, body, head, headSha, base, baseSha, baseSha)
		}
		store.mu.Unlock()
		if p == nil {
			return mutationResult{err: "head or base ref not found"}
		}
		return mutationResult{data: map[string]interface{}{
			"pullRequest": &gqlPull2{repo: repo, p: p},
		}, ok: true}
	case "mergePullRequest":
		arg := firstArgOf(f)
		in, _ := argObj(arg["input"])
		repo, p := findPullByGID(repoIDString(in["pullRequestId"]))
		if p == nil {
			return mutationResult{err: "pull request not found"}
		}
		store.mu.Lock()
		mergable := true
		baseTip, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+p.BaseRef)
		if !ok {
			store.mu.Unlock()
			return mutationResult{err: "base ref not found"}
		}
		baseTree, ok1 := store.commitTree(p.BaseTipAtOpen)
		headTree, ok2 := store.commitTree(p.HeadSha)
		curBaseTree, ok3 := store.commitTree(baseTip)
		if ok1 && ok2 && ok3 {
			baseBlobs := store.flattenTree(baseTree)
			headBlobs := store.flattenTree(headTree)
			curBlobs := store.flattenTree(curBaseTree)
			for path, h := range headBlobs {
				if b, inBase := baseBlobs[path]; inBase && b.oid == h.oid {
					continue
				}
				var mergeBaseOid string
				if b, inBase := baseBlobs[path]; inBase {
					mergeBaseOid = b.oid
				}
				curOid := ""
				if c, inCur := curBlobs[path]; inCur {
					curOid = c.oid
				}
				if mergeBaseOid != curOid {
					mergable = false
					break
				}
			}
			if mergable {
				newTree := curBaseTree
				for path, h := range headBlobs {
					if b, inBase := baseBlobs[path]; inBase && b.oid == h.oid {
						continue
					}
					newTree = store.setPath(newTree, path, h.oid)
				}
				for path := range baseBlobs {
					if _, inHead := headBlobs[path]; !inHead {
						newTree, _ = store.deletePath(newTree, path)
					}
				}
				msg := p.Title
				commitBytes := buildCommitBytes(newTree, []string{baseTip}, msg, VIEWER_NAME, VIEWER_EMAIL, MERGE_COMMIT_TS)
				mergeSha := store.putObject("commit", commitBytes)
				store.setRef(repo.Owner+"/"+repo.Name, "refs/heads/"+p.BaseRef, mergeSha)
				p.State = "closed"
				p.Merged = true
				p.MergeCommit = mergeSha
			}
		}
		store.mu.Unlock()
		if !mergable {
			return mutationResult{err: "Pull Request is not mergeable"}
		}
		return mutationResult{data: map[string]interface{}{
			"pullRequest": &gqlPull2{repo: repo, p: p},
		}, ok: true}
	case "addComment":
		arg := firstArgOf(f)
		in, _ := argObj(arg["input"])
		body, _ := argStr(in["body"])
		subjectID := repoIDString(in["subjectId"])
		store.mu.Lock()
		var comment *gqlComment
		if _, iss := findIssueByGIDLocked(subjectID); iss != nil {
			c := store.addComment(iss.DBID, body)
			comment = &gqlComment{id: commentGID(c.ID), body: c.Body}
		} else if r, p := findPullByGIDLocked(subjectID); p != nil {
			_ = r
			c := store.addComment(p.DBID, body)
			comment = &gqlComment{id: commentGID(c.ID), body: c.Body}
		}
		store.mu.Unlock()
		if comment == nil {
			return mutationResult{err: "subject not found"}
		}
		return mutationResult{data: map[string]interface{}{
			"commentEdge": map[string]interface{}{"node": comment},
		}, ok: true}
	}
	return mutationResult{err: "unknown mutation " + f.name}
}

func repoIDString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case gqlEnumValue:
		return string(t)
	}
	return ""
}

func firstArgOf(f *gqlField) map[string]interface{} {
	// args are on the mutation field itself
	arg := f.args
	if arg == nil {
		return map[string]interface{}{}
	}
	return arg
}

// Locked resolvers for addComment (already holding the store lock).
func findIssueByGIDLocked(id string) (*Repo, *Issue) {
	payload, ok := gqlDecode(id)
	if !ok || !strings.HasPrefix(payload, "Issue:") {
		return nil, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(payload, "Issue:"), ":", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	for _, r := range store.repos {
		if r.ID == repoID {
			return r, store.getIssue(repoID, num)
		}
	}
	return nil, nil
}

func findPullByGIDLocked(id string) (*Repo, *Pull) {
	payload, ok := gqlDecode(id)
	if !ok || !strings.HasPrefix(payload, "PullRequest:") {
		return nil, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(payload, "PullRequest:"), ":", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	for _, r := range store.repos {
		if r.ID == repoID {
			return r, store.getPull(repoID, num)
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- operation execution

type gqlExecResult struct {
	data   interface{}
	errors []map[string]interface{}
}

func execGraphQLOp(op gqlOp, variables map[string]interface{}) gqlExecResult {
	// copy var defaults over provided values
	merged := map[string]interface{}{}
	for k, v := range variables {
		merged[k] = v
	}
	// materialize variables into the op's var defaults is already done at parse time
	_ = merged

	res := gqlExecResult{}
	errs := []map[string]interface{}{}
	rootResolver := func(name string, args map[string]interface{}) (interface{}, bool) {
		switch name {
		case "viewer":
			return &gqlUser{login: VIEWER_LOGIN}, true
		case "repository":
			owner, _ := argStr(args["owner"])
			repoName, _ := argStr(args["name"])
			if r := repoFrom(owner, repoName); r != nil {
				return &gqlRepos{r: r}, true
			}
			return nil, true
		case "node":
			id, _ := argStr(args["id"])
			if r := findRepoByGID(id); r != nil {
				return &gqlRepos{r: r}, true
			}
			return nil, true
		case "__type":
			typeName, _ := argStr(args["name"])
			return gqlTypeObj{name: typeName}, true
		}
		return nil, false
	}

	data := map[string]interface{}{}
	for _, f := range op.fields {
		if op.opType == "mutation" {
			m := execMutation(&f)
			if !m.ok {
				errs = append(errs, map[string]interface{}{"message": m.err})
				data[f.alias] = nil
				continue
			}
			data[f.alias] = selectValue(m.data, projectionOf(f), []interface{}{f.alias}, &errs)
			continue
		}
		val, ok := rootResolver(f.name, f.args)
		if !ok {
			errs = append(errs, map[string]interface{}{"message": fmt.Sprintf("Field '%s' doesn't exist", f.name)})
			data[f.alias] = nil
			continue
		}
		if proj := projectionOf(f); len(proj) > 0 {
			val = selectValue(val, proj, []interface{}{f.alias}, &errs)
		}
		data[f.alias] = val
	}
	res.data = data
	res.errors = errs
	return res
}

// ---------------------------------------------------------------- HTTP

func serveGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "GET" {
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	if !authed(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message": "Bad credentials"}` + "\n"))
		return
	}
	body, err := ioReadAll(r.Body)
	if err != nil {
		jsonWrite(w, 400, map[string]string{"message": "Bad Request"})
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		jsonWrite(w, 400, map[string]string{"message": "Bad Request"})
		return
	}

	// batching: a JSON array of operation requests
	if body[0] == '[' {
		var ops []struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.Unmarshal(body, &ops); err != nil {
			w.Header().Set("Content-Type", "application/json")
			jsonWrite(w, 400, map[string]string{"message": "Bad Request"})
			return
		}
		out := make([]interface{}, 0, len(ops))
		for _, o := range ops {
			out = append(out, runOneOp(o.Query, o.Variables))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", "req-"+requestID())
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
		return
	}

	var req struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonWrite(w, 400, map[string]string{"message": "Bad Request"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", "req-"+requestID())
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(runOneOp(req.Query, req.Variables))
}

func runOneOp(query string, vars map[string]interface{}) map[string]interface{} {
	if vars == nil {
		vars = map[string]interface{}{}
	}
	op, err := parseGQLDoc(query, vars)
	if err != nil {
		return map[string]interface{}{
			"errors": []map[string]interface{}{{"message": err.Error()}},
		}
	}
	res := execGraphQLOp(op, vars)
	if len(res.errors) > 0 {
		data := res.data
		if data == nil {
			data = map[string]interface{}{}
		}
		return map[string]interface{}{
			"data":   data,
			"errors": res.errors,
		}
	}
	return map[string]interface{}{"data": res.data}
}
