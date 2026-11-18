package main

import (
	"sync"
)

type Repo struct {
	ID          int64
	Name        string
	Owner       string
	Private     bool
	Description string
	CreatedAt   int64 // monotonic creation order
}

type Issue struct {
	Number int
	DBID   int64
	Title  string
	Body   string
	State  string // open | closed
	Labels []string
}

type Comment struct {
	ID   int64
	Body string
}

type Pull struct {
	Number        int
	DBID          int64
	Title         string
	Body          string
	State         string // open | closed
	HeadRef       string
	HeadSha       string
	BaseRef       string
	BaseSha       string
	Merged        bool
	MergeCommit   string
	MergedAt      string
	BaseTipAtOpen string // base tip sha at PR-open time, for merge conflict detection
}

type Store struct {
	mu        sync.RWMutex
	objects   map[string]*GitObj
	refs      map[string]string // "refs/heads/..." -> oid
	repos     []*Repo           // creation order
	repoIdx   map[string]*Repo  // owner+"/"+name
	issues    map[int64][]*Issue
	issueNum  map[int64]map[int]*Issue
	nextIssue map[int64]int
	pulls     map[int64][]*Pull
	pullNum   map[int64]map[int]*Pull
	nextPull  map[int64]int
	comments  map[int64][]*Comment // keyed by issue DBID
	labels    map[int64][]string   // keyed by repo ID
	seq       int64
}

func NewStore() *Store {
	return &Store{
		objects:   map[string]*GitObj{},
		refs:      map[string]string{},
		repoIdx:   map[string]*Repo{},
		issues:    map[int64][]*Issue{},
		issueNum:  map[int64]map[int]*Issue{},
		nextIssue: map[int64]int{},
		pulls:     map[int64][]*Pull{},
		pullNum:   map[int64]map[int]*Pull{},
		nextPull:  map[int64]int{},
		comments:  map[int64][]*Comment{},
		labels:    map[int64][]string{},
		seq:       1,
	}
}

// ---------------- objects & refs (caller holds lock) ----------------

func (s *Store) getObject(oid string) (*GitObj, bool) {
	o, ok := s.objects[oid]
	return o, ok
}

func (s *Store) putObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = &GitObj{Type: typ, Data: data}
	}
	return oid
}

func (s *Store) getRef(repoKey, ref string) (string, bool) {
	oid, ok := s.refs[repoKey+"/"+ref]
	return oid, ok
}

func (s *Store) setRef(repoKey, ref, oid string) {
	key := repoKey + "/" + ref
	if oid == "" {
		delete(s.refs, key)
	} else {
		s.refs[key] = oid
	}
}

// reachable computes the closure of objects reachable from roots,
// stopping commit-parent traversal at any commit listed in stopAtParents
// (the client's shallow boundary commits). Returns a set of oids.
func (s *Store) reachable(roots []string, stopAtParents map[string]bool) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj, ok := s.objects[oid]
		if !ok {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			cv := parseCommit(obj.Data)
			queue = append(queue, cv.Tree)
			if !stopAtParents[oid] {
				queue = append(queue, cv.Parents...)
			}
		case "tag":
			// parse tag object: "object <oid>"
			tag := string(obj.Data)
			for _, line := range splitLines(tag) {
				if len(line) > 7 && line[:7] == "object " {
					queue = append(queue, line[7:])
					break
				}
			}
		case "tree":
			for _, e := range parseTree(obj.Data) {
				if e.Mode != "160000" {
					queue = append(queue, e.Oid)
				}
			}
		}
	}
	return seen
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ---------------- repos (caller holds lock) ----------------

func (s *Store) findRepo(owner, name string) *Repo {
	return s.repoIdx[owner+"/"+name]
}

// createRepo makes a new repo owned by the viewer; gives a unique id.
func (s *Store) createRepo(name string, priv bool, desc string) *Repo {
	id := s.seq
	s.seq++
	r := &Repo{ID: id, Name: name, Owner: VIEWER_LOGIN, Private: priv, Description: desc, CreatedAt: id}
	s.repos = append(s.repos, r)
	s.repoIdx[VIEWER_LOGIN+"/"+name] = r
	s.issues[r.ID] = nil
	s.issueNum[r.ID] = map[int]*Issue{}
	s.nextIssue[r.ID] = 1
	s.pulls[r.ID] = nil
	s.pullNum[r.ID] = map[int]*Pull{}
	s.nextPull[r.ID] = 1
	s.labels[r.ID] = nil
	return r
}

func (s *Store) deleteRepo(owner, name string) {
	r := s.findRepo(owner, name)
	if r == nil {
		return
	}
	delete(s.repoIdx, owner+"/"+name)
	for i, cur := range s.repos {
		if cur == r {
			s.repos = append(s.repos[:i], s.repos[i+1:]...)
			break
		}
	}
}

// ---------------- issues (caller holds lock) ----------------

func (s *Store) addIssue(repoID int64, title, body string, labels []string) *Issue {
	num := s.nextIssue[repoID]
	s.nextIssue[repoID] = num + 1
	iss := &Issue{Number: num, DBID: s.seq, Title: title, Body: body, State: "open", Labels: labels}
	s.seq++
	s.issues[repoID] = append(s.issues[repoID], iss)
	s.issueNum[repoID][num] = iss
	return iss
}

func (s *Store) getIssue(repoID int64, number int) *Issue {
	return s.issueNum[repoID][number]
}

// ---------------- pulls (caller holds lock) ----------------

func (s *Store) addPull(repoID int64, title, body, headRef, headSha, baseRef, baseSha, baseTipAtOpen string) *Pull {
	num := s.nextPull[repoID]
	s.nextPull[repoID] = num + 1
	p := &Pull{Number: num, DBID: s.seq, Title: title, Body: body, State: "open",
		HeadRef: headRef, HeadSha: headSha, BaseRef: baseRef, BaseSha: baseSha, BaseTipAtOpen: baseTipAtOpen}
	s.seq++
	s.pulls[repoID] = append(s.pulls[repoID], p)
	s.pullNum[repoID][num] = p
	return p
}

func (s *Store) getPull(repoID int64, number int) *Pull {
	return s.pullNum[repoID][number]
}

// ---------------- comments (caller holds lock) ----------------

func (s *Store) addComment(issueDBID int64, body string) *Comment {
	c := &Comment{ID: s.seq, Body: body}
	s.seq++
	s.comments[issueDBID] = append(s.comments[issueDBID], c)
	return c
}

// ---------------- labels (caller holds lock) ----------------

func (s *Store) addLabel(repoID int64, name string) {
	s.labels[repoID] = append(s.labels[repoID], name)
}

func (s *Store) hasLabel(repoID int64, name string) bool {
	for _, l := range s.labels[repoID] {
		if l == name {
			return true
		}
	}
	return false
}
