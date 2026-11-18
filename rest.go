package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- helpers

func b64encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
func b64decode(s string) []byte {
	s = strings.ReplaceAll(s, "\n", "")
	b, _ := base64.StdEncoding.DecodeString(s)
	return b
}

// repoFrom resolves the repository an owner/name request addresses; 404 when the
// owner's namespace does not hold it (never 403).
func repoFrom(owner, name string) *Repo {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.findRepo(owner, name)
}

// ---------------------------------------------------------------- routing

func serveREST(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v3")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	if p != "/" && p != "/meta" && p != "/user" && p != "/user/repos" && !strings.HasPrefix(p, "/repos/") {
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	// auth: every /api/v3 route requires the bearer token.
	if !requireAuth(w, r) {
		return
	}

	segs := []string{}
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}

	switch {
	case p == "/":
		handleRoot(w, r)
	case p == "/meta":
		handleMeta(w, r)
	case p == "/user" && r.Method == "GET":
		handleUser(w, r)
	case p == "/user/repos" && r.Method == "GET":
		handleListRepos(w, r)
	case p == "/user/repos" && r.Method == "POST":
		handleCreateRepo(w, r)
	case len(segs) >= 2 && segs[0] == "repos":
		if len(segs) < 3 {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		handleRepoRoutes(w, r, segs[1], segs[2], segs[3:])
	default:
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
	}
}

// unquote/unescape a path segment (Go's mux already decodes %2F to "/" within a segment).
func segDec(s string) string { return s }

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-OAuth-Scopes", "repo")
	jsonWrite(w, 200, map[string]string{
		"current_user_url": "/api/v3/user",
	})
}

func handleMeta(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, 200, map[string]string{"installed_version": "makefun-forge/1.0.0"})
}

func handleUser(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, 200, map[string]interface{}{
		"login": VIEWER_LOGIN,
		"id":    VIEWER_ID,
		"name":  VIEWER_NAME,
	})
}

func handleListRepos(w http.ResponseWriter, r *http.Request) {
	store.mu.RLock()
	repos := make([]*Repo, len(store.repos))
	copy(repos, store.repos)
	store.mu.RUnlock()

	perPage := 30
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	start := (page - 1) * perPage
	if start > len(repos) {
		start = len(repos)
	}
	end := start + perPage
	if end > len(repos) {
		end = len(repos)
	}
	out := make([]interface{}, 0, end-start)
	for _, repo := range repos[start:end] {
		out = append(out, repoJSON(repo))
	}
	if page*perPage < len(repos) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		origin := scheme + "://" + r.Host
		if r.Header.Get("Origin") != "" {
			origin = r.Header.Get("Origin")
		}
		next := fmt.Sprintf(`<%s/api/v3/user/repos?per_page=%d&page=%d>; rel="next"`, origin, perPage, page+1)
		w.Header().Set("Link", next)
	}
	jsonWrite(w, 200, out)
}

func repoJSON(r *Repo) map[string]interface{} {
	return map[string]interface{}{
		"id":             r.ID,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]interface{}{"login": r.Owner},
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": DEFAULT_BRANCH,
	}
}

func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Private     bool   `json:"private"`
		Description string `json:"description"`
	}
	if err := readJSONBody(r, &body); err != nil {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	if body.Name == "" {
		errJSON(w, 422, "name is required")
		return
	}
	store.mu.Lock()
	if store.findRepo(VIEWER_LOGIN, body.Name) != nil {
		store.mu.Unlock()
		errJSON(w, 422, "Repository already exists")
		return
	}
	repo := store.createRepo(body.Name, body.Private, body.Description)
	store.mu.Unlock()
	jsonWrite(w, 201, repoJSON(repo))
}

// handleRepoRoutes dispatches sub-routes under /repos/{owner}/{name}.
func handleRepoRoutes(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := repoFrom(owner, name)
	if repo == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			jsonWrite(w, 200, repoJSON(repo))
		case "PATCH":
			var body struct {
				Description *string `json:"description"`
			}
			if err := readJSONBody(r, &body); err != nil {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			store.mu.Lock()
			if body.Description != nil {
				repo.Description = *body.Description
			}
			store.mu.Unlock()
			jsonWrite(w, 200, repoJSON(repo))
		case "DELETE":
			store.mu.Lock()
			store.deleteRepo(owner, name)
			store.mu.Unlock()
			w.WriteHeader(204)
		default:
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
		return
	}

	switch rest[0] {
	case "branches":
		if r.Method == "GET" {
			handleBranches(w, r, repo)
		} else if len(rest) == 2 && r.Method == "GET" {
			handleBranchGet(w, r, repo, rest[1])
		} else {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
	case "commits":
		if r.Method == "GET" {
			handleCommits(w, r, repo)
		} else {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
	case "git":
		handleGitRefs(w, r, repo, rest[1:])
	case "contents":
		handleContents(w, r, repo, rest[1:])
	case "labels":
		handleLabels(w, r, repo, rest[1:])
	case "issues":
		handleIssues(w, r, repo, rest[1:])
	case "pulls":
		handlePulls(w, r, repo, rest[1:])
	default:
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
	}
}

// The current tip of a branch, and whether the branch exists.
func branchTip(repo *Repo, branch string) (string, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	oid, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+branch)
	return oid, ok
}

func handleBranches(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	names := store.sortedRefNames(repo.Owner + "/" + repo.Name)
	type br struct {
		Name string `json:"name"`
		Sha  string `json:"sha"`
	}
	var out []map[string]interface{}
	for _, ref := range names {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		oid, _ := store.getRef(repo.Owner+"/"+repo.Name, ref)
		out = append(out, map[string]interface{}{
			"name":   strings.TrimPrefix(ref, "refs/heads/"),
			"commit": map[string]interface{}{"sha": oid},
		})
	}
	store.mu.RUnlock()
	jsonWrite(w, 200, out)
}

func handleBranchGet(w http.ResponseWriter, r *http.Request, repo *Repo, branch string) {
	oid, ok := branchTip(repo, branch)
	if !ok {
		errJSON(w, 404, "Branch not found")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{
		"name":   branch,
		"commit": map[string]interface{}{"sha": oid},
	})
}

// handleCommits lists ancestors of a sha on the default (or named) branch.
func handleCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	sha := r.URL.Query().Get("sha")
	if sha == "" {
		sha = DEFAULT_BRANCH
	}
	store.mu.RLock()
	oid, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+sha)
	if !ok {
		// sha may itself be a commit id
		if _, ok2 := store.getObject(sha); ok2 {
			oid = sha
		}
	}
	var commits []map[string]interface{}
	if ok || oid != "" {
		seen := map[string]bool{}
		queue := []string{oid}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if seen[cur] {
				continue
			}
			seen[cur] = true
			obj, ok := store.getObject(cur)
			if !ok || obj.Type != "commit" {
				continue
			}
			cv := parseCommit(obj.Data)
			commits = append(commits, map[string]interface{}{
				"sha": cur,
				"commit": map[string]interface{}{
					"message": cv.Message,
					"author":  map[string]interface{}{"name": cv.Author},
				},
			})
			queue = append(queue, cv.Parents...)
		}
	}
	store.mu.RUnlock()
	jsonWrite(w, 200, commits)
}

// handleGitRefs: POST /git/refs and GET /git/refs[/heads/[branch]]
func handleGitRefs(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	// The dispatcher passes path components after "git"; strip the fixed "refs" segment.
	if len(rest) > 0 && rest[0] == "refs" {
		rest = rest[1:]
	}
	switch {
	case len(rest) == 0 && r.Method == "POST":
		var body struct {
			Ref string `json:"ref"`
			Sha string `json:"sha"`
		}
		if err := readJSONBody(r, &body); err != nil {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		if !strings.HasPrefix(body.Ref, "refs/") {
			errJSON(w, 422, "ref must start with refs/")
			return
		}
		body.Sha = strings.ToLower(body.Sha)
		store.mu.Lock()
		_, ok := store.getObject(body.Sha)
		if !ok {
			store.mu.Unlock()
			errJSON(w, 422, "sha is not a known object")
			return
		}
		store.setRef(repo.Owner+"/"+repo.Name, body.Ref, body.Sha)
		store.mu.Unlock()
		jsonWrite(w, 201, map[string]interface{}{
			"ref":    body.Ref,
			"object": map[string]interface{}{"sha": body.Sha},
		})
	case len(rest) == 0 && r.Method == "GET":
		store.mu.RLock()
		var out []map[string]interface{}
		for _, ref := range store.sortedRefNames(repo.Owner + "/" + repo.Name) {
			if ref == "HEAD" {
				continue
			}
			oid, _ := store.getRef(repo.Owner+"/"+repo.Name, ref)
			out = append(out, map[string]interface{}{
				"ref":    "refs/" + ref,
				"object": map[string]interface{}{"sha": oid},
			})
		}
		store.mu.RUnlock()
		jsonWrite(w, 200, out)
	case len(rest) == 2 && rest[0] == "heads" && r.Method == "GET":
		branch := strings.Join(strings.Split(rest[1], "%2F"), "/")
		oid, ok := branchTip(repo, branch)
		if !ok {
			errJSON(w, 404, "Not Found")
			return
		}
		jsonWrite(w, 200, map[string]interface{}{
			"ref":    "refs/heads/" + branch,
			"object": map[string]interface{}{"sha": oid},
		})
	case len(rest) == 2 && rest[0] == "heads" && r.Method == "PATCH":
		var body struct {
			Sha string `json:"sha"`
		}
		if err := readJSONBody(r, &body); err != nil {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		branch := strings.Join(strings.Split(rest[1], "%2F"), "/")
		body.Sha = strings.ToLower(body.Sha)
		store.mu.Lock()
		store.setRef(repo.Owner+"/"+repo.Name, "refs/heads/"+branch, body.Sha)
		store.mu.Unlock()
		jsonWrite(w, 200, map[string]interface{}{
			"ref":    "refs/heads/" + branch,
			"object": map[string]interface{}{"sha": body.Sha},
		})
	default:
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
	}
}

// handleContents: PUT/GET/DELETE /contents/{path...}
func handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	path := strings.Join(rest, "/")
	switch r.Method {
	case "PUT":
		putContent(w, r, repo, path)
	case "GET":
		getContent(w, r, repo, path)
	case "DELETE":
		deleteContent(w, r, repo, path)
	default:
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
	}
}

func putContent(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		Sha     string `json:"sha"`
	}
	if err := readJSONBody(r, &body); err != nil {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	blobBytes := b64decode(body.Content)
	branch := body.Branch
	if branch == "" {
		branch = DEFAULT_BRANCH
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// find the branch's tip tree (unborn ok)
	var tipOid string
	var parentTree string
	if tip, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+branch); ok {
		tipOid = tip
		if obj, ok := store.getObject(tip); ok && obj.Type == "commit" {
			cv := parseCommit(obj.Data)
			parentTree = cv.Tree
		}
	}

	// stale-sha guard: if `sha` provided it must equal the current blob.
	if body.Sha != "" {
		cur, exists := store.getBlob(parentTree, path)
		body.Sha = strings.ToLower(body.Sha)
		if !exists || cur != body.Sha {
			errJSON(w, 409, "sha does not match")
			return
		}
	}

	existed := false
	if _, ok := store.getBlob(parentTree, path); ok {
		existed = true
	}
	blobOid := store.putObject("blob", blobBytes)
	newTree := store.setPath(parentTree, path, blobOid)

	var parents []string
	if tipOid != "" {
		parents = []string{tipOid}
	}
	commitBytes := buildCommitBytes(newTree, parents, body.Message, VIEWER_NAME, VIEWER_EMAIL, MERGE_COMMIT_TS)
	commitOid := store.putObject("commit", commitBytes)
	store.setRef(repo.Owner+"/"+repo.Name, "refs/heads/"+branch, commitOid)

	status := 201
	if existed {
		status = 200
	}
	jsonWrite(w, status, map[string]interface{}{
		"content": map[string]interface{}{
			"sha":      blobOid,
			"encoding": "base64",
		},
		"commit": map[string]interface{}{
			"sha": commitOid,
		},
	})
}

func getContent(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = DEFAULT_BRANCH
	}
	store.mu.RLock()
	defer store.mu.RUnlock()

	tip, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+ref)
	if !ok {
		// ref could be a raw commit sha
		if _, ok2 := store.getObject(ref); ok2 {
			tip = ref
		} else {
			errJSON(w, 404, "Not Found")
			return
		}
	}
	obj, ok := store.getObject(tip)
	if !ok || obj.Type != "commit" {
		errJSON(w, 404, "Not Found")
		return
	}
	cv := parseCommit(obj.Data)
	treeOid := cv.Tree

	// directory listing?
	dirOid, isTree := store.treeOid(treeOid, path)
	if isTree {
		if dirObj, ok := store.getObject(dirOid); ok && dirObj.Type == "tree" {
			entries := parseTree(dirObj.Data)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			out := make([]map[string]interface{}, 0, len(entries))
			for _, e := range entries {
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				}
				out = append(out, map[string]interface{}{
					"type": typ,
					"name": e.Name,
					"path": path + "/" + e.Name,
					"sha":  e.Oid,
				})
			}
			jsonWrite(w, 200, out)
			return
		}
	}

	blobOid, ok := store.getBlob(treeOid, path)
	if !ok {
		errJSON(w, 404, "Not Found")
		return
	}
	blob, ok := store.getObject(blobOid)
	if !ok {
		errJSON(w, 404, "Not Found")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{
		"type":     "file",
		"encoding": "base64",
		"content":  b64encode(string(blob.Data)),
		"sha":      blobOid,
		"size":     len(blob.Data),
		"name":     path[strings.LastIndex(path, "/")+1:],
		"path":     path,
	})
}

func deleteContent(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Branch  string `json:"branch"`
	}
	if err := readJSONBody(r, &body); err != nil {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = DEFAULT_BRANCH
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	tipOid, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+branch)
	var parentTree string
	if ok {
		if obj, ok2 := store.getObject(tipOid); ok2 && obj.Type == "commit" {
			parentTree = parseCommit(obj.Data).Tree
		}
	}
	newTree, existed := store.deletePath(parentTree, path)
	var parents []string
	if tipOid != "" {
		parents = []string{tipOid}
	}
	commitBytes := buildCommitBytes(newTree, parents, body.Message, VIEWER_NAME, VIEWER_EMAIL, MERGE_COMMIT_TS)
	commitOid := store.putObject("commit", commitBytes)
	store.setRef(repo.Owner+"/"+repo.Name, "refs/heads/"+branch, commitOid)

	if !existed {
		errJSON(w, 404, "Not Found")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{
		"commit": map[string]interface{}{"sha": commitOid},
	})
}

func handleLabels(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) > 0 && rest[0] != "" {
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	if r.Method == "POST" {
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := readJSONBody(r, &body); err != nil {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		store.mu.Lock()
		store.addLabel(repo.ID, body.Name)
		store.mu.Unlock()
		jsonWrite(w, 201, map[string]interface{}{"name": body.Name, "color": body.Color})
		return
	}
	store.mu.RLock()
	labels := store.labels[repo.ID]
	out := make([]map[string]interface{}, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]interface{}{"name": l, "color": "ededed"})
	}
	store.mu.RUnlock()
	jsonWrite(w, 200, out)
}

func issueJSON(i *Issue) map[string]interface{} {
	labels := make([]interface{}, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, map[string]interface{}{"name": l})
	}
	return map[string]interface{}{
		"id":     i.DBID,
		"number": i.Number,
		"title":  i.Title,
		"body":   i.Body,
		"state":  i.State,
		"labels": labels,
	}
}

func handleIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		if r.Method == "POST" {
			createIssue(w, r, repo)
		} else if r.Method == "GET" {
			listIssues(w, r, repo)
		} else {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	if len(rest) == 1 {
		store.mu.RLock()
		iss := store.getIssue(repo.ID, n)
		store.mu.RUnlock()
		if iss == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		switch r.Method {
		case "GET":
			jsonWrite(w, 200, issueJSON(iss))
		case "PATCH":
			var body struct {
				State string `json:"state"`
			}
			if err := readJSONBody(r, &body); err != nil {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			store.mu.Lock()
			if body.State != "" {
				iss.State = body.State
			}
			store.mu.Unlock()
			jsonWrite(w, 200, issueJSON(iss))
		default:
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
		return
	}
	if rest[1] == "comments" {
		handleComments(w, r, repo, n)
		return
	}
	jsonWrite(w, 404, map[string]string{"message": "Not Found"})
}

func createIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := readJSONBody(r, &body); err != nil {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	if body.Title == "" {
		errJSON(w, 422, "title is required")
		return
	}
	store.mu.Lock()
	for _, l := range body.Labels {
		if !store.hasLabel(repo.ID, l) {
			store.mu.Unlock()
			errJSON(w, 422, "Validation Failed")
			return
		}
	}
	iss := store.addIssue(repo.ID, body.Title, body.Body, body.Labels)
	store.mu.Unlock()
	jsonWrite(w, 201, issueJSON(iss))
}

func listIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	direction := r.URL.Query().Get("direction")
	store.mu.RLock()
	issues := make([]*Issue, 0)
	for _, i := range store.issues[repo.ID] {
		if state == "open" && i.State != "open" {
			continue
		}
		if state == "closed" && i.State != "closed" {
			continue
		}
		issues = append(issues, i)
	}
	store.mu.RUnlock()
	if direction == "asc" {
		sort.SliceStable(issues, func(a, b int) bool { return issues[a].Number < issues[b].Number })
	} else {
		sort.SliceStable(issues, func(a, b int) bool { return issues[a].Number > issues[b].Number })
	}
	out := make([]map[string]interface{}, 0, len(issues))
	for _, i := range issues {
		out = append(out, issueJSON(i))
	}
	jsonWrite(w, 200, out)
}

func handleComments(w http.ResponseWriter, r *http.Request, repo *Repo, num int) {
	store.mu.RLock()
	iss := store.getIssue(repo.ID, num)
	store.mu.RUnlock()
	if iss == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	switch r.Method {
	case "POST":
		var body struct {
			Body string `json:"body"`
		}
		if err := readJSONBody(r, &body); err != nil {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		store.mu.Lock()
		c := store.addComment(iss.DBID, body.Body)
		store.mu.Unlock()
		jsonWrite(w, 201, map[string]interface{}{"id": c.ID, "body": c.Body})
	case "GET":
		store.mu.RLock()
		comments := store.comments[iss.DBID]
		out := make([]map[string]interface{}, 0, len(comments))
		for _, c := range comments {
			out = append(out, map[string]interface{}{"id": c.ID, "body": c.Body})
		}
		store.mu.RUnlock()
		jsonWrite(w, 200, out)
	default:
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
	}
}

// ---------------------------------------------------------------- pulls

func pullJSON(p *Pull) map[string]interface{} {
	out := map[string]interface{}{
		"number": p.Number,
		"state":  p.State,
		"title":  p.Title,
		"body":   p.Body,
		"head": map[string]interface{}{
			"ref": p.HeadRef,
			"sha": p.HeadSha,
		},
		"base": map[string]interface{}{
			"ref": p.BaseRef,
			"sha": p.BaseSha,
		},
		"merged": p.Merged,
	}
	if p.Merged {
		out["merged_at"] = MERGED_AT
		out["merge_commit_sha"] = p.MergeCommit
	}
	return out
}

func handlePulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		if r.Method == "POST" {
			createPull(w, r, repo)
		} else if r.Method == "GET" {
			listPulls(w, r, repo)
		} else {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	store.mu.RLock()
	pull := store.getPull(repo.ID, n)
	store.mu.RUnlock()
	if len(rest) == 1 {
		if pull == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if r.Method == "GET" {
			jsonWrite(w, 200, pullJSON(pull))
		} else {
			jsonWrite(w, 404, map[string]string{"message": "Not Found"})
		}
		return
	}
	if rest[1] == "files" && r.Method == "GET" {
		if pull == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		jsonWrite(w, 200, pullFiles(repo, pull))
		return
	}
	if rest[1] == "merge" && r.Method == "PUT" {
		if pull == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		mergePull(w, r, repo, pull)
		return
	}
	jsonWrite(w, 404, map[string]string{"message": "Not Found"})
}

func createPull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := readJSONBody(r, &body); err != nil {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	if body.Head == "" || body.Base == "" {
		errJSON(w, 422, "head and base are required")
		return
	}
	store.mu.Lock()
	headSha, hok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+body.Head)
	baseSha, bok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+body.Base)
	if !hok || !bok {
		store.mu.Unlock()
		errJSON(w, 422, "head or base branch does not exist")
		return
	}
	p := store.addPull(repo.ID, body.Title, body.Body, body.Head, headSha, body.Base, baseSha, baseSha)
	store.mu.Unlock()
	jsonWrite(w, 201, pullJSON(p))
}

func listPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	store.mu.RLock()
	pulls := make([]*Pull, 0)
	for _, p := range store.pulls[repo.ID] {
		if state == "all" {
			pulls = append(pulls, p)
		} else if state == "open" && p.State == "open" {
			pulls = append(pulls, p)
		} else if state == "closed" && p.State == "closed" {
			pulls = append(pulls, p)
		}
	}
	store.mu.RUnlock()
	sort.SliceStable(pulls, func(a, b int) bool { return pulls[a].Number < pulls[b].Number })
	out := make([]map[string]interface{}, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, pullJSON(p))
	}
	jsonWrite(w, 200, out)
}

// diff a single path's old/new blobs: additions/deletions/changes.
type fileDiff struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
	Changes   int
}

func countLines(b []byte) []string {
	s := string(b)
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func diffBlob(old, new []byte) (int, int) {
	if old == nil && new == nil {
		return 0, 0
	}
	if old == nil {
		return len(countLines(new)), 0
	}
	if new == nil {
		return 0, len(countLines(old))
	}
	// simple LCS on lines
	a := countLines(old)
	b := countLines(new)
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	lcs := dp[n][m]
	return m - lcs, n - lcs
}

// commitTree resolves the tree oid of a commit.
func (s *Store) commitTree(oid string) (string, bool) {
	obj, ok := s.getObject(oid)
	if !ok || obj.Type != "commit" {
		return "", false
	}
	return parseCommit(obj.Data).Tree, true
}

// treeEntriesWithPrefix returns paths (relative to the tree root) -> blob oid.
type treeBlob struct {
	mode string
	oid  string
}

func (s *Store) flattenTree(treeOid string) map[string]treeBlob {
	out := map[string]treeBlob{}
	var walk func(oid string, prefix string)
	walk = func(oid string, prefix string) {
		obj, ok := s.getObject(oid)
		if !ok || obj.Type != "tree" {
			return
		}
		for _, e := range parseTree(obj.Data) {
			full := e.Name
			if prefix != "" {
				full = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.Oid, full)
			} else {
				out[full] = treeBlob{mode: e.Mode, oid: e.Oid}
			}
		}
	}
	walk(treeOid, "")
	return out
}

// tree from a flat map is done via setPath for simplicity.

func pullFiles(repo *Repo, p *Pull) []map[string]interface{} {
	store.mu.RLock()
	defer store.mu.RUnlock()
	baseTree, ok := store.commitTree(p.BaseTipAtOpen)
	headTree, ok2 := store.commitTree(p.HeadSha)
	if !ok || !ok2 {
		return nil
	}
	baseBlobs := store.flattenTree(baseTree)
	headBlobs := store.flattenTree(headTree)

	paths := map[string]bool{}
	for path := range baseBlobs {
		paths[path] = true
	}
	for path := range headBlobs {
		paths[path] = true
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)

	out := make([]map[string]interface{}, 0)
	for _, path := range names {
		b, inBase := baseBlobs[path]
		h, inHead := headBlobs[path]
		if inBase && inHead && b.oid == h.oid {
			continue
		}
		var oldB, newB []byte
		if inBase {
			if obj, ok := store.getObject(b.oid); ok {
				oldB = obj.Data
			}
		}
		if inHead {
			if obj, ok := store.getObject(h.oid); ok {
				newB = obj.Data
			}
		}
		add, del := diffBlob(oldB, newB)
		status := "modified"
		if !inBase {
			status = "added"
		} else if !inHead {
			status = "removed"
		}
		out = append(out, map[string]interface{}{
			"filename":  path,
			"status":    status,
			"additions": add,
			"deletions": del,
			"changes":   add + del,
		})
	}
	return out
}

// mergePull performs a squash merge (also used for merge/REBASE in practice).
func mergePull(w http.ResponseWriter, r *http.Request, repo *Repo, p *Pull) {
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if err := readJSONBody(r, &body); err == nil {
		// method accepted; squash is the only implemented strategy
		_ = body.MergeMethod
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if p.State != "open" {
		errJSON(w, 405, "Pull Request is not mergeable")
		return
	}
	baseTip, ok := store.getRef(repo.Owner+"/"+repo.Name, "refs/heads/"+p.BaseRef)
	if !ok {
		errJSON(w, 404, "Not Found")
		return
	}
	baseTree, ok1 := store.commitTree(p.BaseTipAtOpen)
	headTree, ok2 := store.commitTree(p.HeadSha)
	if !ok1 || !ok2 {
		errJSON(w, 404, "Not Found")
		return
	}
	curBaseTree, ok3 := store.commitTree(baseTip)
	if !ok3 {
		errJSON(w, 404, "Not Found")
		return
	}

	baseBlobs := store.flattenTree(baseTree)
	headBlobs := store.flattenTree(headTree)
	curBlobs := store.flattenTree(curBaseTree)

	// conflict detection: a head-changed path whose base version moved since open.
	for path, h := range headBlobs {
		b, inBase := baseBlobs[path]
		if inBase && b.oid == h.oid {
			continue // unchanged since merge base
		}
		// changed (added/modified/deleted): compare merge-base version to current base
		var mergeBaseOid string
		if inBase {
			mergeBaseOid = b.oid
		}
		c, inCur := curBlobs[path]
		curOid := ""
		if inCur {
			curOid = c.oid
		}
		if mergeBaseOid != curOid {
			errJSON(w, 405, "Pull Request is not mergeable")
			return
		}
	}

	// apply head changes onto the current base tree
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

	jsonWrite(w, 200, map[string]interface{}{
		"sha":     mergeSha,
		"merged":  true,
		"message": msg,
	})
}
