package main

import (
	"encoding/hex"
	"sort"
	"strings"
)

// ---- tree addressing helpers (caller holds the store lock) ----

func splitPath(p string) []string {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// treeOid resolves the oid of the tree at path within treeOid ("" when absent).
func (s *Store) treeOid(treeOid, path string) (string, bool) {
	segs := splitPath(path)
	if len(segs) == 0 {
		return treeOid, true
	}
	cur := treeOid
	for _, seg := range segs {
		obj, ok := s.getObject(cur)
		if !ok || obj.Type != "tree" {
			return "", false
		}
		next := ""
		for _, e := range parseTree(obj.Data) {
			if e.Name == seg {
				next = e.Oid
				break
			}
		}
		if next == "" {
			return "", false
		}
		cur = next
	}
	return cur, true
}

// getBlob resolves the blob oid at a path in the given tree ("" when absent).
func (s *Store) getBlob(treeOid, path string) (string, bool) {
	segs := splitPath(path)
	if len(segs) == 0 {
		return "", false
	}
	dir := strings.Join(segs[:len(segs)-1], "/")
	leaf := segs[len(segs)-1]
	dirOid, ok := s.treeOid(treeOid, dir)
	if !ok {
		return "", false
	}
	obj, ok := s.getObject(dirOid)
	if !ok || obj.Type != "tree" {
		return "", false
	}
	for _, e := range parseTree(obj.Data) {
		if e.Name == leaf && e.Mode != "40000" {
			return e.Oid, true
		}
	}
	return "", false
}

// setPath returns a new tree oid with path -> blobOid, creating intervening trees.
func (s *Store) setPath(treeOid, path, blobOid string) string {
	segs := splitPath(path)
	var build func(parentOid string, idx int) string
	build = func(parentOid string, idx int) string {
		// parentOid may be "" for an unborn tree.
		var entries []TreeEntry
		if parentOid != "" {
			if obj, ok := s.getObject(parentOid); ok && obj.Type == "tree" {
				entries = parseTree(obj.Data)
			}
		}
		name := segs[idx]
		if idx == len(segs)-1 {
			// leaf: replace or append a blob entry
			replaced := false
			for i := range entries {
				if entries[i].Name == name {
					mode := entries[i].Mode
					if mode == "40000" {
						mode = "100644"
					}
					entries[i] = TreeEntry{Mode: mode, Name: name, Oid: blobOid}
					replaced = true
					break
				}
			}
			if !replaced {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, Oid: blobOid})
			}
			return s.putObject("tree", buildTree(entries))
		}
		// directory: recurse
		childOid := ""
		for _, e := range entries {
			if e.Name == name {
				childOid = e.Oid
				break
			}
		}
		newChild := build(childOid, idx+1)
		found := false
		for i := range entries {
			if entries[i].Name == name {
				entries[i] = TreeEntry{Mode: "40000", Name: name, Oid: newChild}
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: newChild})
		}
		return s.putObject("tree", buildTree(entries))
	}
	return build(treeOid, 0)
}

// deletePath returns a new tree oid with path removed (identity when absent).
func (s *Store) deletePath(treeOid, path string) (string, bool) {
	segs := splitPath(path)
	if len(segs) == 0 || treeOid == "" {
		return treeOid, false
	}
	var removed bool
	var build func(parentOid string, idx int) string
	build = func(parentOid string, idx int) string {
		var entries []TreeEntry
		if obj, ok := s.getObject(parentOid); ok && obj.Type == "tree" {
			entries = parseTree(obj.Data)
		}
		name := segs[idx]
		var out []TreeEntry
		if idx == len(segs)-1 {
			for _, e := range entries {
				if e.Name == name {
					removed = true
					continue
				}
				out = append(out, e)
			}
		} else {
			for _, e := range entries {
				if e.Name == name && e.Mode == "40000" {
					newChild := build(e.Oid, idx+1)
					obj, _ := s.getObject(newChild)
					entries2 := parseTree(obj.Data)
					if len(entries2) > 0 {
						out = append(out, TreeEntry{Mode: "40000", Name: name, Oid: newChild})
					}
				} else {
					out = append(out, e)
				}
			}
		}
		return s.putObject("tree", buildTree(out))
	}
	newTree := build(treeOid, 0)
	return newTree, removed
}

type dirEntry struct {
	Name string
	Mode string
	Oid  string
}

// listDir returns the immediate children of a directory path, sorted by name.
func (s *Store) listDir(treeOid, dirPath string) []dirEntry {
	dirOid, ok := s.treeOid(treeOid, dirPath)
	if !ok {
		return nil
	}
	obj, ok := s.getObject(dirOid)
	if !ok || obj.Type != "tree" {
		return nil
	}
	entries := parseTree(obj.Data)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, dirEntry{Name: e.Name, Mode: e.Mode, Oid: e.Oid})
	}
	return out
}

// treeAt returns the raw oid of a subtree at a path if it is a tree.
func (s *Store) treeAt(treeOid, path string) (string, bool) {
	return s.treeOid(treeOid, path)
}

func oidHex(raw []byte) string { return hex.EncodeToString(raw) }
