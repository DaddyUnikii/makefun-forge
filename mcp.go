package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const MCP_REVISION = "2026-07-28"

// ---------------------------------------------------------------- JSON-RPC plumbing

func mcpSetHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", MCP_REVISION)
	w.Header().Set("X-GitHub-Request-Id", "req-"+requestID())
}

func mcpWrite(w http.ResponseWriter, id interface{}, result interface{}) {
	mcpSetHeaders(w)
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func mcpErr(w http.ResponseWriter, id interface{}, code int, msg string) {
	mcpSetHeaders(w)
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": msg},
	})
}

// serveMCP is the Streamable HTTP (2026-07-28) MCP endpoint.
func serveMCP(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		mcpSetHeaders(w)
		jsonWrite(w, 405, map[string]string{"message": "Method Not Allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		mcpErr(w, nil, -32700, "Parse error")
		return
	}
	var req struct {
		Method string                 `json:"method"`
		ID     interface{}            `json:"id"`
		Params map[string]interface{} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil {
		mcpSetHeaders(w)
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]interface{}{"code": -32700, "message": "Parse error"},
		})
		return
	}
	switch req.Method {
	case "server/discover":
		mcpWrite(w, req.ID, mcpDiscover())
	case "tools/list":
		mcpWrite(w, req.ID, mcpToolList())
	case "tools/call":
		name, _ := req.Params["name"].(string)
		if !isKnownMCPTool(name) {
			mcpErr(w, req.ID, -32601, "Unknown tool: "+name)
			return
		}
		args, _ := req.Params["arguments"].(map[string]interface{})
		res, isErr := mcpCallTool(name, args)
		mcpWrite(w, req.ID, map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": jsonString(res)},
			},
			"structuredContent": res,
			"isError":           isErr,
		})
	default:
		mcpErr(w, req.ID, -32601, "Method not found")
	}
}

func jsonString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func isKnownMCPTool(name string) bool {
	switch name {
	case "create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request":
		return true
	}
	return false
}

func mcpDiscover() map[string]interface{} {
	return map[string]interface{}{
		"resultType":        "discover",
		"supportedVersions": []string{MCP_REVISION},
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{"listChanged": true},
		},
		"serverInfo": map[string]interface{}{"name": "github-forge", "version": "1.0.0"},
		"_meta": map[string]interface{}{
			"io.modelcontextprotocol/protocolVersion": MCP_REVISION,
		},
	}
}

type mcpToolDef struct {
	Name        string                 `json:"name"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func mcpToolList() map[string]interface{} {
	tools := []mcpToolDef{
		{"create_repo", "Create a repository", "Create a new repository owned by the authenticated user.",
			mcpSchema(map[string]interface{}{
				"name":        map[string]interface{}{"type": "string"},
				"private":     map[string]interface{}{"type": "boolean"},
				"description": map[string]interface{}{"type": "string"},
			}, []string{"name"})},
		{"get_repo", "Get a repository", "Return a repository by owner and name.",
			mcpSchema(map[string]interface{}{
				"owner": map[string]interface{}{"type": "string"},
				"repo":  map[string]interface{}{"type": "string"},
			}, []string{"owner", "repo"})},
		{"create_issue", "Create an issue", "Create a new issue in a repository.",
			mcpSchema(map[string]interface{}{
				"owner":  map[string]interface{}{"type": "string"},
				"repo":   map[string]interface{}{"type": "string"},
				"title":  map[string]interface{}{"type": "string"},
				"body":   map[string]interface{}{"type": "string"},
				"labels": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"owner", "repo", "title"})},
		{"list_issues", "List issues", "List issues in a repository, optionally filtered by state.",
			mcpSchema(map[string]interface{}{
				"owner": map[string]interface{}{"type": "string"},
				"repo":  map[string]interface{}{"type": "string"},
				"state": map[string]interface{}{"type": "string"},
			}, []string{"owner", "repo"})},
		{"create_pull_request", "Create a pull request", "Create a pull request from a head branch into a base branch.",
			mcpSchema(map[string]interface{}{
				"owner": map[string]interface{}{"type": "string"},
				"repo":  map[string]interface{}{"type": "string"},
				"title": map[string]interface{}{"type": "string"},
				"body":  map[string]interface{}{"type": "string"},
				"head":  map[string]interface{}{"type": "string"},
				"base":  map[string]interface{}{"type": "string"},
			}, []string{"owner", "repo", "title", "head", "base"})},
		{"merge_pull_request", "Merge a pull request", "Merge a pull request into its base branch.",
			mcpSchema(map[string]interface{}{
				"owner":       map[string]interface{}{"type": "string"},
				"repo":        map[string]interface{}{"type": "string"},
				"pull_number": map[string]interface{}{"type": "integer"},
			}, []string{"owner", "repo", "pull_number"})},
	}
	return map[string]interface{}{
		"resultType": "toolsList",
		"tools":      tools,
	}
}

func mcpSchema(props map[string]interface{}, required []string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// ---------------------------------------------------------------- tool dispatch

func mcpArgStr(args map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func mcpArgInt(args map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			n, _ := v.Int64()
			return int(n)
		}
	}
	return 0
}

func mcpCallTool(name string, args map[string]interface{}) (interface{}, bool) {
	switch name {
	case "create_repo":
		return mcpCreateRepo(args)
	case "get_repo":
		return mcpGetRepo(args)
	case "create_issue":
		return mcpCreateIssue(args)
	case "list_issues":
		return mcpListIssues(args)
	case "create_pull_request":
		return mcpCreatePR(args)
	case "merge_pull_request":
		return mcpMergePR(args)
	default:
		return map[string]interface{}{"error": "unknown tool: " + name}, true
	}
}

func repoView(r *Repo) map[string]interface{} {
	return map[string]interface{}{
		"id":          r.ID,
		"name":        r.Name,
		"full_name":   r.Owner + "/" + r.Name,
		"owner":       map[string]interface{}{"login": r.Owner},
		"private":     r.Private,
		"description": r.Description,
	}
}

func issueView(i *Issue) map[string]interface{} {
	return map[string]interface{}{
		"number": i.Number,
		"title":  i.Title,
		"body":   i.Body,
		"state":  i.State,
		"labels": i.Labels,
	}
}

func mcpCreateRepo(args map[string]interface{}) (interface{}, bool) {
	name := mcpArgStr(args, "name")
	if name == "" {
		return map[string]interface{}{"message": "name is required"}, true
	}
	priv, _ := args["private"].(bool)
	desc := mcpArgStr(args, "description")
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.findRepo(VIEWER_LOGIN, name) != nil {
		return map[string]interface{}{"message": "repository already exists"}, true
	}
	r := store.createRepo(name, priv, desc)
	return repoView(r), false
}

func mcpGetRepo(args map[string]interface{}) (interface{}, bool) {
	owner := mcpArgStr(args, "owner", "owner_login", "org", "user")
	name := mcpArgStr(args, "repo", "repository", "repo_name", "repo_slug", "project")
	store.mu.RLock()
	defer store.mu.RUnlock()
	r := store.findRepo(owner, name)
	if r == nil {
		return map[string]interface{}{"message": "Not Found"}, true
	}
	return repoView(r), false
}

func mcpCreateIssue(args map[string]interface{}) (interface{}, bool) {
	owner := mcpArgStr(args, "owner", "owner_login", "org", "user")
	name := mcpArgStr(args, "repo", "repository", "repo_name", "repo_slug", "project")
	title := mcpArgStr(args, "title")
	body := mcpArgStr(args, "body", "issue_body", "description", "content", "text", "comment")
	var labels []string
	if lv, ok := args["labels"].([]interface{}); ok {
		for _, l := range lv {
			if s, ok := l.(string); ok {
				labels = append(labels, s)
			}
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	r := store.findRepo(owner, name)
	if r == nil {
		return map[string]interface{}{"message": "Not Found"}, true
	}
	iss := store.addIssue(r.ID, title, body, labels)
	return issueView(iss), false
}

func mcpListIssues(args map[string]interface{}) (interface{}, bool) {
	owner := mcpArgStr(args, "owner", "owner_login", "org", "user")
	name := mcpArgStr(args, "repo", "repository", "repo_name", "repo_slug", "project")
	state := mcpArgStr(args, "state")
	store.mu.RLock()
	r := store.findRepo(owner, name)
	if r == nil {
		store.mu.RUnlock()
		return map[string]interface{}{"message": "Not Found"}, true
	}
	issues := store.issues[r.ID]
	store.mu.RUnlock()
	out := []interface{}{}
	for _, i := range issues {
		if state != "" && state != "all" && i.State != state {
			continue
		}
		out = append(out, issueView(i))
	}
	return map[string]interface{}{"issues": out, "total_count": len(out)}, false
}

func mcpCreatePR(args map[string]interface{}) (interface{}, bool) {
	owner := mcpArgStr(args, "owner", "owner_login", "org", "user")
	name := mcpArgStr(args, "repo", "repository", "repo_name", "repo_slug", "project")
	title := mcpArgStr(args, "title")
	body := mcpArgStr(args, "body", "pull_request_body", "description", "content", "text")
	head := mcpArgStr(args, "head", "head_ref", "head_branch", "pull_request_head", "pr_head", "source")
	base := mcpArgStr(args, "base", "base_ref", "base_branch", "pull_request_base", "pr_base", "target")

	store.mu.Lock()
	defer store.mu.Unlock()
	r := store.findRepo(owner, name)
	if r == nil {
		return map[string]interface{}{"message": "Not Found"}, true
	}
	headSha, okHead := store.getRef(owner+"/"+name, "refs/heads/"+head)
	baseSha, okBase := store.getRef(owner+"/"+name, "refs/heads/"+base)
	if !okHead || !okBase {
		return map[string]interface{}{"message": "Not Found"}, true
	}
	p := store.addPull(r.ID, title, body, head, headSha, base, baseSha, baseSha)
	return map[string]interface{}{
		"number": p.Number,
		"title":  p.Title,
		"body":   p.Body,
		"state":  p.State,
		"head":   map[string]interface{}{"ref": p.HeadRef, "sha": p.HeadSha},
		"base":   map[string]interface{}{"ref": p.BaseRef, "sha": p.BaseSha},
		"merged": p.Merged,
	}, false
}

func mcpMergePR(args map[string]interface{}) (interface{}, bool) {
	owner := mcpArgStr(args, "owner", "owner_login", "org", "user")
	name := mcpArgStr(args, "repo", "repository", "repo_name", "repo_slug", "project")
	number := mcpArgInt(args, "pull_number", "pull_request_number", "pr_number", "number", "issue_number", "index")

	store.mu.Lock()
	defer store.mu.Unlock()
	r := store.findRepo(owner, name)
	if r == nil {
		return map[string]interface{}{"message": "Not Found"}, true
	}
	p := store.getPull(r.ID, number)
	if p == nil {
		return map[string]interface{}{"message": "Not Found"}, true
	}
	if p.State != "open" || p.Merged {
		return map[string]interface{}{"message": "Pull Request is not mergeable"}, true
	}
	mergeSha, ok := storeMergePullLocked(r, p)
	if !ok {
		return map[string]interface{}{"message": "Pull Request is not mergeable"}, true
	}
	return map[string]interface{}{
		"number":           p.Number,
		"merged":           true,
		"merge_commit_sha": mergeSha,
		"sha":              mergeSha,
	}, false
}

// storeMergePullLocked mirrors rest.go's mergePull, returning (mergeSha, ok).
// Caller holds store.mu.
func storeMergePullLocked(r *Repo, p *Pull) (string, bool) {
	baseTip, ok := store.getRef(r.Owner+"/"+r.Name, "refs/heads/"+p.BaseRef)
	if !ok {
		return "", false
	}
	baseTree, ok1 := store.commitTree(p.BaseTipAtOpen)
	headTree, ok2 := store.commitTree(p.HeadSha)
	curBaseTree, ok3 := store.commitTree(baseTip)
	if !ok1 || !ok2 || !ok3 {
		return "", false
	}
	baseBlobs := store.flattenTree(baseTree)
	headBlobs := store.flattenTree(headTree)
	curBlobs := store.flattenTree(curBaseTree)

	for path, h := range headBlobs {
		b, inBase := baseBlobs[path]
		if inBase && b.oid == h.oid {
			continue
		}
		var mergeBaseOid string
		if inBase {
			mergeBaseOid = b.oid
		}
		curOid := ""
		if c, inCur := curBlobs[path]; inCur {
			curOid = c.oid
		}
		if mergeBaseOid != curOid {
			return "", false
		}
	}

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

	commitBytes := buildCommitBytes(newTree, []string{baseTip}, p.Title, VIEWER_NAME, VIEWER_EMAIL, MERGE_COMMIT_TS)
	mergeSha := store.putObject("commit", commitBytes)
	store.setRef(r.Owner+"/"+r.Name, "refs/heads/"+p.BaseRef, mergeSha)
	p.State = "closed"
	p.Merged = true
	p.MergeCommit = mergeSha
	return mergeSha, true
}

// ensure fmt is used even if a future edit drops a format call.
var _ = fmt.Sprintf
