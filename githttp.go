package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ---- pkt-line codec ----

func pkt(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// readPktLines parses pkt-line framed data, returning payloads until (and
// excluding) the flush packet, plus the bytes after the flush.
func readPktLines(data []byte) (lines [][]byte, rest []byte) {
	pos := 0
	for {
		if pos+4 > len(data) {
			break
		}
		head := string(data[pos : pos+4])
		pos += 4
		if head == "0000" {
			break
		}
		if head == "0001" || head == "0002" {
			continue
		}
		var n int
		fmt.Sscanf(head, "%04x", &n)
		if n < 4 {
			break
		}
		if pos+n-4 > len(data) {
			lines = append(lines, data[pos:])
			pos = len(data)
			break
		}
		lines = append(lines, data[pos:pos+n-4])
		pos += n - 4
	}
	return lines, data[pos:]
}

func isObjectIdLike(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ---- ref advertisement ----

// sortedRefNames returns this repo's ref names in a stable order (HEAD first).
func (s *Store) sortedRefNames(repoKey string) []string {
	var names []string
	s.mu.RLock()
	prefix := repoKey + "/"
	for n := range s.refs {
		if strings.HasPrefix(n, prefix) {
			names = append(names, strings.TrimPrefix(n, prefix))
		}
	}
	hasMain := s.refs[repoKey+"/refs/heads/"+DEFAULT_BRANCH]
	s.mu.RUnlock()
	// sort by (has "/", name)
	sort.Slice(names, func(i, j int) bool {
		return refSortKey(names[i]) < refSortKey(names[j])
	})
	// prepend HEAD if main exists, else prepend unborn HEAD
	head := ""
	if hasMain != "" {
		head = hasMain
	}
	if head != "" {
		names = append([]string{"HEAD"}, names...)
	}
	return names
}

func refSortKey(name string) string {
	// put branches before tags for determinism, then lexicographic
	if strings.HasPrefix(name, "refs/heads/") {
		return "0" + name
	}
	return "1" + name
}

// peelTag resolves an annotated tag oid to the commit it ultimately points at.
func (s *Store) peelTag(oid string) string {
	seen := map[string]bool{}
	for {
		if seen[oid] {
			return ""
		}
		seen[oid] = true
		obj, ok := s.objects[oid]
		if !ok {
			return ""
		}
		if obj.Type == "tag" {
			tag := string(obj.Data)
			for _, line := range splitLines(tag) {
				if strings.HasPrefix(line, "object ") {
					oid = strings.TrimPrefix(line, "object ")
					break
				}
			}
			continue
		}
		return oid // commit (or other) id
	}
}

// ---- info/refs (smart HTTP advertisement) ----

func (s *Store) serveInfoRefs(w http.ResponseWriter, r *http.Request, owner, repo, service string) {
	if !s.hasRepoRW(owner, repo) {
		w.WriteHeader(404)
		w.Write([]byte("Not Found"))
		return
	}
	rk := owner + "/" + repo
	gitProtocol := r.Header.Get("Git-Protocol")
	wantV2 := service == "git-upload-pack" && strings.Contains(gitProtocol, "version=2")

	if service == "git-receive-pack" {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(s.receiveAdvertisement(rk))
		return
	}
	// upload-pack
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	if wantV2 {
		w.Write(s.uploadPackV2Advertisement())
	} else {
		w.Write(s.uploadPackV0Advertisement(rk))
	}
}

func (s *Store) uploadPackV2Advertisement() []byte {
	var b bytes.Buffer
	b.Write(pkt("version 2\n"))
	b.Write(pkt("agent=makefun-forge/1.0\n"))
	b.Write(pkt("ls-refs=unborn\n"))
	b.Write(pkt("fetch=shallow\n"))
	b.Write(pkt("object-format=sha1\n"))
	b.Write(pktFlush())
	return b.Bytes()
}

func (s *Store) uploadPackV0Advertisement(repoKey string) []byte {
	var b bytes.Buffer
	b.Write(pkt("# service=git-upload-pack\n"))
	b.Write(pktFlush())
	b.Write(s.refAdvertisement("upload", repoKey))
	return b.Bytes()
}

func (s *Store) receiveAdvertisement(repoKey string) []byte {
	var b bytes.Buffer
	b.Write(pkt("# service=git-receive-pack\n"))
	b.Write(pktFlush())
	b.Write(s.refAdvertisement("receive", repoKey))
	return b.Bytes()
}

// refAdvertisement emits the v0 ref list with capabilities on the first line.
func (s *Store) refAdvertisement(service, repoKey string) []byte {
	names := s.sortedRefNames(repoKey)
	caps := ""
	if service == "receive" {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0 object-format=sha1"
	} else {
		caps = "side-band-64k multi_ack symref=HEAD:refs/heads/" + DEFAULT_BRANCH + " shallow ofs-delta agent=makefun-forge/1.0 object-format=sha1"
	}
	var b bytes.Buffer
	first := true
	s.mu.RLock()
	for _, name := range names {
		oid := s.refs[repoKey+"/"+name]
		if name == "HEAD" {
			// HEAD is a synthetic symref, not a stored key; resolve its target.
			oid = s.refs[repoKey+"/refs/heads/"+DEFAULT_BRANCH]
		}
		var line string
		if name == "HEAD" {
			line = oid + " HEAD"
		} else {
			line = oid + " " + name
		}
		if first {
			line += "\x00" + caps + "\n"
			first = false
		} else {
			line += "\n"
		}
		b.Write(pkt(line))
		// peeled tags in v0: emit the ^{} line for tags
		if strings.HasPrefix(name, "refs/tags/") {
			peeled := s.peelTagLocked(oid)
			if peeled != "" && peeled != oid {
				if first {
					b.Write(pkt(peeled + " " + name + "^{}\x00" + caps + "\n"))
					first = false
				} else {
					b.Write(pkt(peeled + " " + name + "^{}\n"))
				}
			}
		}
	}
	s.mu.RUnlock()
	// Empty repository: advertise capabilities on a synthetic "capabilities^{}"
	// pseudo-ref (zero oid), as real git does, so the client sees side-band-64k
	// and report-status before its first push.
	if first {
		b.Write(pkt(ZERO_OID + " capabilities^{}\x00" + caps + "\n"))
	}
	b.Write(pktFlush())
	return b.Bytes()
}

func (s *Store) peelTagLocked(oid string) string {
	seen := map[string]bool{}
	for {
		if seen[oid] {
			return ""
		}
		seen[oid] = true
		obj, ok := s.objects[oid]
		if !ok {
			return ""
		}
		if obj.Type == "tag" {
			tag := string(obj.Data)
			found := false
			for _, line := range splitLines(tag) {
				if strings.HasPrefix(line, "object ") {
					oid = strings.TrimPrefix(line, "object ")
					found = true
					break
				}
			}
			if !found {
				return ""
			}
			continue
		}
		return oid
	}
}

func (s *Store) hasRepoRW(owner, name string) bool {
	if owner != VIEWER_LOGIN {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repoIdx[VIEWER_LOGIN+"/"+name] != nil
}

// ---- upload-pack (fetch + ls-refs, protocol v2 + v0) ----

func (s *Store) serveUploadPack(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if !s.hasRepoRW(owner, repo) {
		w.WriteHeader(404)
		return
	}
	body, err := ioReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	lines, _ := readPktLines(body)
	if len(lines) == 0 {
		// malformed request (repo-survives-errors sends "zzzznot-a-pkt-line")
		w.WriteHeader(200)
		w.Write(pktFlush())
		return
	}
	firstLine := string(bytes.TrimSpace(lines[0]))
	switch {
	case strings.HasPrefix(firstLine, "command=ls-refs"):
		s.serveLsRefs(owner+"/"+repo, w, lines)
	case strings.HasPrefix(firstLine, "command=fetch"):
		s.serveFetchV2(w, lines)
	default:
		// v0 fetch: lines are want/have/flush sections
		s.serveFetchV0(w, body)
	}
}

func (s *Store) serveLsRefs(repoKey string, w http.ResponseWriter, lines [][]byte) {
	// parse arguments after the delim
	var prefixes []string
	var peel, symrefs, unborn bool
	sawDelim := false
	for _, ln := range lines {
		text := strings.TrimRight(string(ln), "\n")
		if text == "command=ls-refs" || text == "agent" || strings.HasPrefix(text, "agent=") ||
			strings.HasPrefix(text, "object-format") {
			continue
		}
		if text == "" && !sawDelim {
			continue
		}
		// delim marked by empty pkt (0001) — but readPktLines skips delim; detect via content
		switch {
		case text == "peel":
			peel = true
		case text == "symrefs":
			symrefs = true
		case text == "unborn":
			unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(text, "ref-prefix "))
		}
		sawDelim = true
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var b bytes.Buffer
	s.mu.RLock()
	names := s.sortedRefNames(repoKey)
	mainTip, hasMain := s.refs[repoKey+"/refs/heads/"+DEFAULT_BRANCH]
	s.mu.RUnlock()

	emit := func(oid, name, extra string) {
		b.Write(pkt(fmt.Sprintf("%s %s%s\n", oid, name, extra)))
	}
	_ = peel
	_ = symrefs
	_ = unborn

	// Build the set of refs to emit (with prefix filtering).
	type refLine struct {
		oid, name, symref, peeled string
		unborn                    bool
	}
	var out []refLine
	hasHead := false
	s.mu.RLock()
	for _, name := range names {
		if name == "HEAD" {
			continue
		}
		oid := s.refs[repoKey+"/"+name]
		var rl refLine
		rl.oid = oid
		rl.name = name
		if strings.HasPrefix(name, "refs/tags/") {
			rl.peeled = s.peelTagLocked(oid)
		}
		out = append(out, rl)
	}
	// HEAD line
	if hasMain {
		headLine := refLine{oid: mainTip, name: "HEAD", symref: "refs/heads/" + DEFAULT_BRANCH}
		out = append([]refLine{headLine}, out...)
		hasHead = true
	}
	s.mu.RUnlock()
	_ = hasHead

	// apply prefix filter
	filtered := out
	if len(prefixes) > 0 {
		filtered = nil
		for _, rl := range out {
			for _, p := range prefixes {
				if strings.HasPrefix(rl.name, p) {
					filtered = append(filtered, rl)
					break
				}
			}
		}
	}

	for _, rl := range filtered {
		if rl.name == "HEAD" {
			if rl.symref != "" {
				if !hasMain {
					// unborn
					emit("unborn", "HEAD", " symref-target:"+rl.symref)
					continue
				}
				emit(rl.oid, "HEAD", " symref-target:"+rl.symref)
			} else {
				emit(rl.oid, "HEAD", "")
			}
			continue
		}
		extra := ""
		if rl.peeled != "" && rl.peeled != rl.oid {
			extra += " peeled:" + rl.peeled
		}
		emit(rl.oid, rl.name, extra)
	}
	b.Write(pktFlush())
	w.Write(b.Bytes())
}

func (s *Store) serveFetchV2(w http.ResponseWriter, lines [][]byte) {
	var wants, haves, shallow []string
	var deepen int = -1
	var deepenNot []string
	for _, ln := range lines {
		text := string(ln)
		trim := strings.TrimRight(text, "\n")
		switch {
		case strings.HasPrefix(trim, "want "):
			wants = append(wants, strings.TrimPrefix(trim, "want "))
		case strings.HasPrefix(trim, "have "):
			haves = append(haves, strings.TrimPrefix(trim, "have "))
		case strings.HasPrefix(trim, "shallow "):
			shallow = append(shallow, strings.TrimPrefix(trim, "shallow "))
		case strings.HasPrefix(trim, "deepen "):
			fmt.Sscanf(strings.TrimPrefix(trim, "deepen "), "%d", &deepen)
		case strings.HasPrefix(trim, "deepen-not "):
			deepenNot = append(deepenNot, strings.TrimPrefix(trim, "deepen-not "))
		}
	}

	pack, shallowOut, unshallowOut := s.computeFetch(wants, haves, shallow, deepen, deepenNot)

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var b bytes.Buffer
	b.Write(pkt("acknowledgments\n"))
	b.Write(pkt("NAK\n"))
	b.Write(pktDelim())
	if len(shallowOut) > 0 || len(unshallowOut) > 0 {
		b.Write(pkt("shallow-info\n"))
		for _, o := range shallowOut {
			b.Write(pkt("shallow " + o + "\n"))
		}
		for _, o := range unshallowOut {
			b.Write(pkt("unshallow " + o + "\n"))
		}
		b.Write(pktDelim())
	}
	b.Write(pkt("packfile\n"))
	// sideband packets (band 1), 64k chunks. The pkt-line total (4-byte header +
	// 1-byte band + data) must stay at or under LARGE_PACKET_MAX (65520).
	const chunk = 65515
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		b.Write(pkt(string(append([]byte{1}, pack[i:end]...))))
	}
	b.Write(pktFlush())
	w.Write(b.Bytes())
}

// serveFetchV0 handles a v0 upload-pack POST (want/have/flush/done sections).
func (s *Store) serveFetchV0(w http.ResponseWriter, body []byte) {
	lines, _ := readPktLines(body)
	var wants, haves []string
	for _, ln := range lines {
		trim := strings.TrimSpace(string(ln))
		switch {
		case strings.HasPrefix(trim, "want "):
			wants = append(wants, strings.TrimPrefix(trim, "want "))
		case strings.HasPrefix(trim, "have "):
			haves = append(haves, strings.TrimPrefix(trim, "have "))
		}
	}
	pack, _, _ := s.computeFetch(wants, haves, nil, -1, nil)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var b bytes.Buffer
	b.Write(pkt("NAK\n"))
	const chunk = 65515
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		b.Write(pkt(string(append([]byte{1}, pack[i:end]...))))
	}
	b.Write(pktFlush())
	w.Write(b.Bytes())
}

// computeFetch computes the pack and shallow-info for a fetch.
func (s *Store) computeFetch(wants, haves, shallow []string, deepen int, deepenNot []string) ([]byte, []string, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	shallowSet := map[string]bool{}
	for _, o := range shallow {
		shallowSet[o] = true
	}

	// have commits: full reachable from haves, stopping parent traversal at shallow boundaries.
	haveAll := map[string]bool{}
	for _, h := range haves {
		for oid := range s.reachableLocked([]string{h}, shallowSet) {
			haveAll[oid] = true
		}
	}
	haveCommits := map[string]bool{}
	for _, h := range haves {
		if isObjectIdLike(h) {
			haveCommits[h] = true
			for c := range s.ancestorsCommitted(h, shallowSet) {
				haveCommits[c] = true
			}
		}
	}

	// wanted commits (depth-limited walk from wants)
	wantedCommits := map[string]bool{}
	if deepen >= 0 {
		type item struct {
			oid   string
			depth int
		}
		queue := []item{}
		for _, w := range wants {
			queue = append(queue, item{w, 0})
		}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur.depth >= deepen {
				continue
			}
			if wantedCommits[cur.oid] {
				continue
			}
			obj, ok := s.objects[cur.oid]
			if !ok || obj.Type != "commit" {
				continue
			}
			wantedCommits[cur.oid] = true
			cv := parseCommit(obj.Data)
			for _, p := range cv.Parents {
				queue = append(queue, item{p, cur.depth + 1})
			}
		}
	} else {
		// full history from wants, stopping at the client's own shallow boundaries.
		for _, w := range wants {
			for c := range s.ancestorsCommitted(w, shallowSet) {
				wantedCommits[c] = true
			}
			// also include the want itself even if not a commit (unlikely)
			if obj, ok := s.objects[w]; ok && obj.Type == "commit" {
				wantedCommits[w] = true
			}
		}
	}

	// shallow boundaries: wanted commits whose parent is neither wanted nor held.
	// Computed before the pack so the pack stops at the parent of a boundary commit.
	var shallowOut []string
	for c := range wantedCommits {
		obj, ok := s.objects[c]
		if !ok {
			continue
		}
		cv := parseCommit(obj.Data)
		for _, p := range cv.Parents {
			if !wantedCommits[p] && !haveCommits[p] {
				shallowOut = append(shallowOut, c)
				break
			}
		}
	}
	// unshallow: the client's own shallow commits whose parents are now all available.
	unshallowed := map[string]bool{}
	var unshallowOut []string
	for o := range shallowSet {
		obj, ok := s.objects[o]
		if !ok || obj.Type != "commit" {
			continue
		}
		cv := parseCommit(obj.Data)
		allAvail := true
		for _, p := range cv.Parents {
			if !wantedCommits[p] && !haveCommits[p] {
				allAvail = false
				break
			}
		}
		if allAvail {
			unshallowOut = append(unshallowOut, o)
			unshallowed[o] = true
		}
	}

	// boundary: commits where parent traversal must stop — the new shallow
	// boundaries plus the client's own shallow commits it is not deepening past.
	boundary := map[string]bool{}
	for _, c := range shallowOut {
		boundary[c] = true
	}
	for o := range shallowSet {
		if !unshallowed[o] {
			boundary[o] = true
		}
	}

	// objects to send: closure of wanted commits and any wanted tag objects,
	// stopping commit-parent traversal at the boundary, minus haveAll.
	send := map[string]bool{}
	roots := map[string]bool{}
	for c := range wantedCommits {
		roots[c] = true
	}
	for _, w := range wants {
		if obj, ok := s.objects[w]; ok && obj.Type == "tag" {
			roots[w] = true
		}
	}
	for c := range roots {
		for oid := range s.reachableLocked([]string{c}, boundary) {
			send[oid] = true
		}
	}
	for oid := range haveAll {
		delete(send, oid)
	}
	sort.Strings(shallowOut)
	sort.Strings(unshallowOut)

	// build object list in deterministic order
	var oids []string
	for oid := range send {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objs := make([]*GitObj, 0, len(oids))
	for _, oid := range oids {
		if obj, ok := s.objects[oid]; ok {
			objs = append(objs, obj)
		}
	}
	return writePack(objs), shallowOut, unshallowOut
}

// reachableLocked is reachable() but assumes the store lock is held.
func (s *Store) reachableLocked(roots []string, stopAtParents map[string]bool) map[string]bool {
	return s.reachable(roots, stopAtParents)
}

// ancestorsCommitted returns the set of commit oids reachable from tip (commit nodes only),
// stopping parent traversal at shallow boundaries.
func (s *Store) ancestorsCommitted(tip string, shallowSet map[string]bool) map[string]bool {
	out := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[cur] {
			continue
		}
		obj, ok := s.objects[cur]
		if !ok || obj.Type != "commit" {
			continue
		}
		out[cur] = true
		if shallowSet[cur] {
			continue
		}
		cv := parseCommit(obj.Data)
		queue = append(queue, cv.Parents...)
	}
	return out
}

// ---- receive-pack (v0 push) ----

func (s *Store) serveReceivePack(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if !s.hasRepoRW(owner, repo) {
		w.WriteHeader(404)
		return
	}
	rk := owner + "/" + repo
	body, err := ioReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	lines, rest := readPktLines(body)

	type cmd struct {
		oldOid, newOid, ref string
	}
	var cmds []cmd
	for _, ln := range lines {
		text := string(ln)
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		text = strings.TrimRight(text, "\n")
		if text == "" {
			continue
		}
		parts := strings.SplitN(text, " ", 3)
		if len(parts) != 3 {
			continue
		}
		cmds = append(cmds, cmd{parts[0], parts[1], parts[2]})
	}

	// unpack the pack (if any)
	unpackDetail := "ok"
	if len(rest) > 0 {
		objs, perr := readPack(rest)
		if perr != nil {
			unpackDetail = perr.Error()
		} else {
			s.mu.Lock()
			for oid, obj := range objs {
				if _, exists := s.objects[oid]; !exists {
					s.objects[oid] = obj
				}
			}
			s.mu.Unlock()
		}
	}

	// apply ref updates per command
	var report []string
	report = append(report, fmt.Sprintf("unpack %s", unpackDetail))
	s.mu.Lock()
	for _, c := range cmds {
		current, exists := s.refs[rk+"/"+c.ref]
		ok := false
		reason := ""
		switch {
		case c.newOid == ZERO_OID:
			// delete
			if exists && current == c.oldOid {
				delete(s.refs, rk+"/"+c.ref)
				ok = true
			} else if exists && current != c.oldOid {
				reason = "non-fast-forward"
			} else if !exists {
				reason = "non-fast-forward"
			}
		case c.oldOid == ZERO_OID:
			// create
			if !exists {
				// ensure objects present (already unpacked) — accept
				s.refs[rk+"/"+c.ref] = c.newOid
				ok = true
			} else if current == c.newOid {
				ok = true
			} else {
				ok = true
				s.refs[rk+"/"+c.ref] = c.newOid
			}
		default:
			// update: old must equal current, and new must be a descendant of old
			if !exists {
				reason = "non-fast-forward"
			} else if current != c.oldOid {
				reason = "non-fast-forward"
			} else if !s.isDescendantLocked(c.oldOid, c.newOid) {
				reason = "non-fast-forward"
			} else {
				s.refs[rk+"/"+c.ref] = c.newOid
				ok = true
			}
		}
		if ok {
			report = append(report, "ok "+c.ref)
		} else {
			report = append(report, fmt.Sprintf("ng %s %s", c.ref, reason))
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var b bytes.Buffer
	for _, line := range report {
		b.Write(pkt(string([]byte{1}) + line + "\n"))
	}
	b.Write(pktFlush())
	w.Write(b.Bytes())
}

// isDescendantLocked reports whether ancestor is reachable from tip (commits only).
func (s *Store) isDescendantLocked(ancestor, tip string) bool {
	visited := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if cur == ancestor {
			return true
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		obj, ok := s.objects[cur]
		if !ok || obj.Type != "commit" {
			continue
		}
		cv := parseCommit(obj.Data)
		queue = append(queue, cv.Parents...)
	}
	return false
}

func ioReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), nil
		}
	}
}
