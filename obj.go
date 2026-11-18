package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const ZERO_OID = "0000000000000000000000000000000000000000"

// Scenario-derived constants (wire.ts buildScenario, SCENARIO_SEED fixed).
const VIEWER_LOGIN = "adaiversen"
const VIEWER_NAME = "Ada Iversen"
const VIEWER_EMAIL = "adaiversen@example.invalid"
const VIEWER_ID = 1000
const DEFAULT_BRANCH = "main"
const MERGED_AT = "2026-11-01T00:00:00Z"

// Deterministic commit timestamp (the scenario epoch; never the wall clock).
const MERGE_COMMIT_TS int64 = 1735689600

type GitObj struct {
	Type string // commit, tree, blob, tag
	Data []byte
}

// hashObject computes the git object id: SHA-1 over "<type> <len>\x00" + data.
func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

// parseTree decodes a git tree object ("mode name\0<20 raw oid>" * n).
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := -1
		for i := off; i < len(data); i++ {
			if data[i] == ' ' {
				sp = i
				break
			}
		}
		if sp < 0 {
			break
		}
		nul := -1
		for i := sp + 1; i < len(data); i++ {
			if data[i] == 0 {
				nul = i
				break
			}
		}
		if nul < 0 || nul+21 > len(data) {
			break
		}
		mode := string(data[off:sp])
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{mode, name, oid})
		off = nul + 21
	}
	return out
}

// buildTree serializes entries in git's tree sort order (dirs compared as "name/").
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	nameOf := func(e TreeEntry) string {
		if e.Mode == "40000" {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(sorted, func(i, j int) bool { return nameOf(sorted[i]) < nameOf(sorted[j]) })
	var b []byte
	for _, e := range sorted {
		b = append(b, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.Oid)
		b = append(b, raw...)
	}
	return b
}

type CommitView struct {
	Tree    string
	Parents []string
	Author  string
	Message string
}

// parseCommit decodes a commit object's headers and message.
func parseCommit(data []byte) CommitView {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	header := text
	msg := ""
	if blank >= 0 {
		header = text[:blank]
		msg = text[blank+2:]
	}
	cv := CommitView{Message: msg}
	for _, line := range strings.Split(header, "\n") {
		if line == "" {
			continue
		}
		sp := strings.Index(line, " ")
		var key, val string
		if sp < 0 {
			key = line
		} else {
			key = line[:sp]
			val = line[sp+1:]
		}
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		}
	}
	return cv
}

// buildCommit serializes a commit object deterministically.
func buildCommitBytes(tree string, parents []string, message string, authorName, authorEmail string, ts int64) []byte {
	who := fmt.Sprintf("%s <%s> %d +0000", authorName, authorEmail, ts)
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(tree)
	b.WriteString("\n")
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("author ")
	b.WriteString(who)
	b.WriteString("\ncommitter ")
	b.WriteString(who)
	b.WriteString("\n\n")
	b.WriteString(message)
	return []byte(b.String())
}

// isAncestor reports whether ancestor is reachable from tip following first-parent
// and all parents of commit objects in the store.
func (s *Store) isAncestor(lookup func(string) (*GitObj, bool), ancestor, tip string, depthLimit int) bool {
	// BFS from tip; memoized visited set.
	visited := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == ancestor {
			return true
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		obj, ok := lookup(cur)
		if !ok || obj.Type != "commit" {
			continue
		}
		cv := parseCommit(obj.Data)
		if depthLimit > 0 && len(queue) > depthLimit*4 {
			break
		}
		for _, p := range cv.Parents {
			queue = append(queue, p)
		}
	}
	return false
}
