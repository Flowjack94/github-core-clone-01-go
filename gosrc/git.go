// Git smart HTTP: info/refs advertisements (v2 and v0), ls-refs and fetch
// (v2 and v0), receive-pack. Packets are pkt-lines; packs are plain
// objects only (no deltas).
package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "go-forge/1.0"

// ---- pkt-lines ----------------------------------------------------------

func pktLine(s string) []byte {
	n := len(s) + 4
	var b []byte
	b = append(b, hexDigit(n>>12), hexDigit(n>>8), hexDigit(n>>4), hexDigit(n))
	b = append(b, s...)
	return b
}

func hexDigit(v int) byte {
	const digits = "0123456789abcdef"
	return digits[v&0xf]
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLines splits a body into packets: each is ("data", payload) or
// ("flush"/"delim", nil). Returns false when the body is not pkt-framed.
func pktLines(body []byte) ([]pkt, bool) {
	var out []pkt
	i := 0
	for i < len(body) {
		if i+4 > len(body) {
			return nil, false
		}
		n, err := strconv.ParseUint(string(body[i:i+4]), 16, 32)
		if err != nil {
			return nil, false
		}
		if n == 0 {
			out = append(out, pkt{kind: "flush"})
			i += 4
			continue
		}
		if n == 1 {
			out = append(out, pkt{kind: "delim"})
			i += 4
			continue
		}
		if n < 4 || i+int(n) > len(body) {
			return nil, false
		}
		out = append(out, pkt{kind: "data", payload: body[i+4 : i+int(n)]})
		i += int(n)
	}
	return out, true
}

type pkt struct {
	kind    string // data | flush | delim
	payload []byte
}

// gitResp builds a git content-type response.
func gitResp(contentType string, body []byte) []byte {
	return newResp(200, contentType, body)
}

// ---- routing ------------------------------------------------------------

// routeGit handles /{owner}/{repo}.git/<tail...>.
func (s *server) routeGit(req *request, owner, repo string, tail []string) []byte {
	if len(tail) == 0 {
		return errResp(404, "Not Found")
	}
	r := s.lookupRepo(owner, repo)
	if r == nil {
		return errResp(404, "Not Found")
	}
	switch {
	case len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" && req.method == "GET":
		return s.gitInfoRefs(req, r)
	case len(tail) == 1 && tail[0] == "git-upload-pack" && req.method == "POST":
		return s.gitUploadPack(req, r)
	case len(tail) == 1 && tail[0] == "git-receive-pack" && req.method == "POST":
		return s.gitReceivePack(req, r)
	}
	return errResp(404, "Not Found")
}

// sortedRefNames returns the repo's ref names in deterministic order:
// branches (refs/heads/*) sorted, then tags (refs/tags/*) sorted.
func (r *Repo) sortedRefNames() []string {
	var heads, tags []string
	for name := range r.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, name)
		} else if strings.HasPrefix(name, "refs/tags/") {
			tags = append(tags, name)
		}
	}
	sort.Strings(heads)
	sort.Strings(tags)
	return append(heads, tags...)
}

// ---- info/refs ----------------------------------------------------------

func (s *server) gitInfoRefs(req *request, r *Repo) []byte {
	service := req.query("service")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	switch service {
	case "git-upload-pack":
		if strings.Contains(req.header("git-protocol"), "version=2") {
			body := s.advertiseV2Upload(r)
			return gitResp("application/x-git-upload-pack-advertisement", body)
		}
		body := s.advertiseV0(r, "git-upload-pack")
		return gitResp("application/x-git-upload-pack-advertisement", body)
	case "git-receive-pack":
		body := s.advertiseV0(r, "git-receive-pack")
		return gitResp("application/x-git-receive-pack-advertisement", body)
	}
	return errResp(400, "unknown service")
}

func (s *server) advertiseV2Upload(r *Repo) []byte {
	var b []byte
	b = append(b, pktLine("# service=git-upload-pack\n")...)
	b = append(b, pktFlush...)
	b = append(b, pktLine("version 2\n")...)
	b = append(b, pktLine("ls-refs=unborn\n")...)
	b = append(b, pktLine("fetch=shallow\n")...)
	b = append(b, pktLine("object-format=sha1\n")...)
	b = append(b, pktLine("agent="+gitAgent+"\n")...)
	b = append(b, pktFlush...)
	return b
}

// advertiseV0 renders a v0 advertisement for either service.
func (s *server) advertiseV0(r *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k shallow thin-pack ofs-delta symref=HEAD:refs/heads/" + r.DefaultBranch + " agent=" + gitAgent
	} else {
		caps = "report-status side-band-64k delete-refs agent=" + gitAgent
	}
	var b []byte
	b = append(b, pktLine("# service="+service+"\n")...)
	b = append(b, pktFlush...)
	headOID := r.defaultBranchOID()
	names := r.sortedRefNames()
	if headOID == "" && len(names) == 0 {
		b = append(b, pktLine(zeroOID+" capabilities^{}\x00"+caps+"\n")...)
		b = append(b, pktFlush...)
		return b
	}
	if headOID == "" {
		headOID = zeroOID
	}
	b = append(b, pktLine(headOID+" HEAD\x00"+caps+"\n")...)
	for _, name := range names {
		oid := r.Refs[name]
		b = append(b, pktLine(oid+" "+name+"\n")...)
		if strings.HasPrefix(name, "refs/tags/") {
			if peeled := r.peeledTag(oid); peeled != "" {
				b = append(b, pktLine(peeled+" "+name+"^{}\n")...)
			}
		}
	}
	b = append(b, pktFlush...)
	return b
}

// peeledTag returns the commit a tag object points at, or "".
func (r *Repo) peeledTag(tagOID string) string {
	obj := r.Objects[tagOID]
	if obj == nil || obj.Type != "tag" {
		return ""
	}
	// tag object: "object <oid>\ntype ...\n..."
	for _, line := range bytes.Split(obj.Data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			return string(line[7:])
		}
	}
	return ""
}

// ---- upload-pack --------------------------------------------------------

// gitUploadPack handles POST git-upload-pack: ls-refs/fetch v2, or fetch v0.
func (s *server) gitUploadPack(req *request, r *Repo) []byte {
	packets, ok := pktLines(req.body)
	if !ok || len(packets) == 0 || packets[0].kind != "data" {
		return errResp(400, "bad pkt-line stream")
	}
	first := string(packets[0].payload)
	if strings.HasPrefix(first, "command=") {
		cmd := strings.TrimSuffix(strings.TrimPrefix(first, "command="), "\n")
		switch cmd {
		case "ls-refs":
			return s.gitLsRefs(req, r, packets[1:])
		case "fetch":
			return s.gitFetchV2(req, r, packets[1:])
		}
		return errResp(400, "unknown command "+cmd)
	}
	if strings.HasPrefix(first, "want ") {
		return s.gitFetchV0(req, r, packets)
	}
	return errResp(400, "unexpected upload-pack request")
}

// lsArgs are the arguments after the command line (delim separates the
// command's own arguments from the request arguments).
type lsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func parseLsArgs(packets []pkt) lsArgs {
	var a lsArgs
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			a.peel = true
		case line == "symrefs":
			a.symrefs = true
		case line == "unborn":
			a.unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			a.prefixes = append(a.prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	return a
}

func (s *server) gitLsRefs(req *request, r *Repo, packets []pkt) []byte {
	args := parseLsArgs(packets)
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	keep := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, p := range args.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b []byte
	// HEAD line
	headOID := r.defaultBranchOID()
	if keep("HEAD") {
		if headOID == "" {
			line := "unborn HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/" + r.DefaultBranch
			}
			b = append(b, pktLine(line+"\n")...)
		} else {
			line := headOID + " HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/" + r.DefaultBranch
			}
			b = append(b, pktLine(line+"\n")...)
		}
	}
	for _, name := range r.sortedRefNames() {
		if !keep(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if args.peel {
			if peeled := r.peeledTag(oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		b = append(b, pktLine(line+"\n")...)
	}
	b = append(b, pktFlush...)
	return gitResp("application/x-git-upload-pack-result", b)
}

// fetchArgs are a fetch request's parsed arguments.
type fetchArgs struct {
	wants    []string
	haves    []string
	shallows []string
	depth    int // 0 = no deepen
}

func parseFetchArgs(packets []pkt) fetchArgs {
	var a fetchArgs
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			a.wants = append(a.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			a.haves = append(a.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			a.shallows = append(a.shallows, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				a.depth = n
			}
		}
	}
	return a
}

// fetchPlan is the computed object set for a fetch.
type fetchPlan struct {
	objects   []*Object // objects to send, deterministic order
	shallow   []string  // new shallow boundary commits
	unshallow []string  // old boundaries now included
	acks      []string  // haves present in the store
	nak       bool
}

// planFetch computes what a fetch sends. held stops at the client's
// shallow boundaries; with depth, only generations < depth are sent.
func (r *Repo) planFetch(args fetchArgs) fetchPlan {
	var plan fetchPlan
	// held: closure of haves (commits, trees, blobs), not crossing the
	// client's shallow boundaries.
	held := make(map[string]bool)
	shallowSet := make(map[string]bool)
	for _, s := range args.shallows {
		shallowSet[s] = true
	}
	{
		// BFS over commits from haves; collect all reachable objects.
		var queue []string
		for _, h := range args.haves {
			if r.Objects[h] != nil {
				queue = append(queue, h)
			}
		}
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if held[oid] {
				continue
			}
			held[oid] = true
			obj := r.Objects[oid]
			if obj == nil {
				continue
			}
			switch obj.Type {
			case "commit":
				c := parseCommit(obj.Data)
				if obj2 := r.Objects[c.Tree]; obj2 != nil {
					queue = append(queue, c.Tree)
				}
				if !shallowSet[oid] {
					queue = append(queue, c.Parents...)
				}
			case "tree":
				if entries, err := parseTree(obj.Data); err == nil {
					for _, e := range entries {
						queue = append(queue, e.OID)
					}
				}
			}
		}
	}
	// acks: haves present in the store
	for _, h := range args.haves {
		if r.Objects[h] != nil {
			plan.acks = append(plan.acks, h)
		}
	}
	plan.nak = len(args.haves) == 0

	// Walk wants with generations. Tag wants contribute the tag object
	// itself and peel to the commit it points at.
	type genCommit struct {
		oid string
		gen int
	}
	wantCommits := make(map[string]int) // oid -> generation
	var tagObjs []*Object
	{
		queue := []genCommit{}
		for _, w := range args.wants {
			obj := r.Objects[w]
			if obj == nil {
				continue
			}
			if obj.Type == "tag" {
				// peel through tags to the commit, collecting each tag
				cur := w
				for {
					o := r.Objects[cur]
					if o == nil || o.Type != "tag" {
						break
					}
					tagObjs = append(tagObjs, o)
					target := ""
					for _, line := range bytes.Split(o.Data, []byte("\n")) {
						if bytes.HasPrefix(line, []byte("object ")) {
							target = string(line[7:])
							break
						}
					}
					if target == "" {
						break
					}
					cur = target
				}
				if r.Objects[cur] != nil && r.Objects[cur].Type == "commit" {
					queue = append(queue, genCommit{cur, 0})
				}
				continue
			}
			if obj.Type == "commit" {
				queue = append(queue, genCommit{w, 0})
			}
		}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if prev, ok := wantCommits[cur.oid]; ok && prev <= cur.gen {
				continue
			}
			wantCommits[cur.oid] = cur.gen
			if args.depth > 0 && cur.gen >= args.depth-1 {
				continue // parents are beyond the depth
			}
			for _, p := range r.commitParents(cur.oid) {
				queue = append(queue, genCommit{p, cur.gen + 1})
			}
		}
	}
	// shallow boundaries: included commits at generation depth-1
	if args.depth > 0 {
		for oid, gen := range wantCommits {
			if gen == args.depth-1 {
				plan.shallow = append(plan.shallow, oid)
			}
		}
		sort.Strings(plan.shallow)
	}
	// unshallow: client boundaries now included
	for _, s := range args.shallows {
		if _, ok := wantCommits[s]; ok {
			plan.unshallow = append(plan.unshallow, s)
		}
	}
	sort.Strings(plan.unshallow)

	// Collect objects: commits, then their trees/blobs.
	send := make(map[string]bool)
	var commits []*Object
	var trees []*Object
	var blobs []*Object
	addTree := func(treeOID string) {
		// recursive tree walk (iterative)
		stack := []string{treeOID}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if send[oid] || held[oid] {
				continue
			}
			obj := r.Objects[oid]
			if obj == nil || obj.Type != "tree" {
				continue
			}
			send[oid] = true
			trees = append(trees, obj)
			entries, err := parseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir {
					stack = append(stack, e.OID)
				} else if !send[e.OID] && !held[e.OID] {
					if b := r.Objects[e.OID]; b != nil {
						send[e.OID] = true
						blobs = append(blobs, b)
					}
				}
			}
		}
	}
	// deterministic commit order: generation, then oid
	type cg struct {
		oid string
		gen int
	}
	var ordered []cg
	for oid, gen := range wantCommits {
		ordered = append(ordered, cg{oid, gen})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].gen != ordered[j].gen {
			return ordered[i].gen < ordered[j].gen
		}
		return ordered[i].oid < ordered[j].oid
	})
	for _, c := range ordered {
		if send[c.oid] || held[c.oid] {
			continue
		}
		obj := r.Objects[c.oid]
		if obj == nil {
			continue
		}
		send[c.oid] = true
		commits = append(commits, obj)
		addTree(parseCommit(obj.Data).Tree)
	}
	// Tag objects wanted directly (not held) go first, then commits,
	// trees, blobs.
	var tags []*Object
	for _, o := range tagObjs {
		oid := hashObject(o.Type, o.Data)
		if !send[oid] && !held[oid] {
			send[oid] = true
			tags = append(tags, o)
		}
	}
	plan.objects = append(append(append(tags, commits...), trees...), blobs...)
	return plan
}

// wantRef returns the first ref name pointing at oid (heads first).
func (r *Repo) wantRef(oid string) string {
	for _, name := range r.sortedRefNames() {
		if r.Refs[name] == oid {
			return name
		}
	}
	return ""
}

func (s *server) gitFetchV2(req *request, r *Repo, packets []pkt) []byte {
	args := parseFetchArgs(packets)
	s.store.mu.RLock()
	plan := r.planFetch(args)
	s.store.mu.RUnlock()
	var b []byte
	// acknowledgments: real git sends this section ONLY when the client
	// sent haves (otherwise the section is omitted entirely — a client
	// that sent no haves expects the next section, not a NAK).
	if len(args.haves) > 0 {
		b = append(b, pktLine("acknowledgments\n")...)
		for _, a := range plan.acks {
			b = append(b, pktLine("ACK "+a+"\n")...)
		}
		b = append(b, pktLine("ready\n")...)
		b = append(b, pktDelim...)
	}
	// shallow-info (only when there are lines to report)
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		b = append(b, pktLine("shallow-info\n")...)
		for _, sh := range plan.shallow {
			b = append(b, pktLine("shallow "+sh+"\n")...)
		}
		for _, un := range plan.unshallow {
			b = append(b, pktLine("unshallow "+un+"\n")...)
		}
		b = append(b, pktDelim...)
	}
	// wanted-refs: real git omits this section for a plain fetch (the
	// client already knows the refs from ls-refs); the env parser treats
	// every section as optional, so omitting it is safe on both sides.
	// packfile (omitted entirely when nothing to send)
	if len(plan.objects) > 0 {
		pack := writePack(plan.objects)
		b = append(b, pktLine("packfile\n")...)
		b = append(b, sidebandPack(pack)...)
	}
	b = append(b, pktFlush...)
	return gitResp("application/x-git-upload-pack-result", b)
}

// sidebandPack frames pack bytes as band-1 sideband pkt-lines.
func sidebandPack(pack []byte) []byte {
	var b []byte
	for len(pack) > 0 {
		n := len(pack)
		if n > 65515 {
			n = 65515
		}
		chunk := pack[:n]
		pack = pack[n:]
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, chunk...)
		b = append(b, pktLineBytes(payload)...)
	}
	return b
}

// pktLineBytes frames raw payload bytes as one pkt-line.
func pktLineBytes(payload []byte) []byte {
	n := len(payload) + 4
	var b []byte
	b = append(b, hexDigit(n>>12), hexDigit(n>>8), hexDigit(n>>4), hexDigit(n))
	b = append(b, payload...)
	return b
}

func (s *server) gitFetchV0(req *request, r *Repo, packets []pkt) []byte {
	// v0: wants section (first line may carry capabilities), then haves/done.
	// parseFetchArgs handles both sections uniformly.
	args := parseFetchArgs(packets)
	s.store.mu.RLock()
	plan := r.planFetch(args)
	s.store.mu.RUnlock()
	var b []byte
	if plan.nak {
		b = append(b, pktLine("NAK\n")...)
	} else {
		for _, a := range plan.acks {
			b = append(b, pktLine("ACK "+a+"\n")...)
		}
	}
	for _, sh := range plan.shallow {
		b = append(b, pktLine("shallow "+sh+"\n")...)
	}
	for _, un := range plan.unshallow {
		b = append(b, pktLine("unshallow "+un+"\n")...)
	}
	if len(plan.objects) > 0 {
		pack := writePack(plan.objects)
		b = append(b, sidebandPack(pack)...)
	}
	b = append(b, pktFlush...)
	return gitResp("application/x-git-upload-pack-result", b)
}

// ---- receive-pack -------------------------------------------------------

// gitReceivePack handles POST git-receive-pack.
func (s *server) gitReceivePack(req *request, r *Repo) []byte {
	// Parse pkt-lines up to the first flush packet; everything after it is
	// the raw pack (not pkt-framed), so it must not be parsed as pkt-lines.
	var commands []pushCmd
	pos := 0
	body := req.body
	packStart := -1
	for pos < len(body) {
		if pos+4 > len(body) {
			return errResp(400, "bad pkt-line stream")
		}
		n, err := strconv.ParseUint(string(body[pos:pos+4]), 16, 32)
		if err != nil {
			return errResp(400, "bad pkt-line stream")
		}
		if n == 0 {
			packStart = pos + 4
			break
		}
		if n == 1 {
			pos += 4
			continue
		}
		if n < 4 || pos+int(n) > len(body) {
			return errResp(400, "bad pkt-line stream")
		}
		line := string(body[pos+4 : pos+int(n)])
		pos += int(n)
		// strip capabilities on the first line
		if idx := strings.IndexByte(line, 0); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return errResp(400, "bad push command")
		}
		commands = append(commands, pushCmd{old: parts[0], new: parts[1], ref: parts[2]})
	}
	if packStart < 0 {
		return errResp(400, "bad pkt-line stream")
	}
	if len(commands) == 0 {
		return errResp(400, "no push commands")
	}
	packBytes := body[packStart:]
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	// Validate + store the pack (only when a command carries objects).
	needsPack := false
	for _, c := range commands {
		if c.new != zeroOID {
			needsPack = true
		}
	}
	if needsPack {
		if len(packBytes) == 0 {
			return s.pushReport(r, commands, nil, "unpack error: missing packfile")
		}
		objs, err := parsePack(packBytes)
		if err != nil {
			return s.pushReport(r, commands, nil, "unpack error: "+err.Error())
		}
		for _, o := range objs {
			r.storeObject(o.typ, o.data)
		}
	}
	// Apply the commands in order.
	var outcomes []outcome
	for _, c := range commands {
		cur, exists := r.Refs[c.ref]
		if c.new == zeroOID {
			// delete
			if !exists {
				outcomes = append(outcomes, outcome{c.ref, false, "remote ref does not exist"})
				continue
			}
			if cur != c.old {
				outcomes = append(outcomes, outcome{c.ref, false, "non-fast-forward"})
				continue
			}
			delete(r.Refs, c.ref)
			if strings.HasPrefix(c.ref, "refs/heads/") {
				br := strings.TrimPrefix(c.ref, "refs/heads/")
				for i, b := range r.Branches {
					if b == br {
						r.Branches = append(r.Branches[:i], r.Branches[i+1:]...)
						break
					}
				}
			}
			outcomes = append(outcomes, outcome{c.ref, true, ""})
			continue
		}
		// create or update: the new object must be a commit (branches) or
		// a tag object (tag refs); lightweight tags point at commits.
		obj := r.Objects[c.new]
		if obj == nil || (obj.Type != "commit" && obj.Type != "tag") {
			outcomes = append(outcomes, outcome{c.ref, false, "missing object"})
			continue
		}
		if !exists {
			if c.old != zeroOID {
				outcomes = append(outcomes, outcome{c.ref, false, "non-fast-forward"})
				continue
			}
			r.Refs[c.ref] = c.new
			if strings.HasPrefix(c.ref, "refs/heads/") {
				r.Branches = append(r.Branches, strings.TrimPrefix(c.ref, "refs/heads/"))
			}
			outcomes = append(outcomes, outcome{c.ref, true, ""})
			continue
		}
		if cur != c.old {
			outcomes = append(outcomes, outcome{c.ref, false, "non-fast-forward"})
			continue
		}
		// Tag refs are replaced, not fast-forwarded.
		if !strings.HasPrefix(c.ref, "refs/tags/") && !r.isAncestor(cur, c.new) {
			outcomes = append(outcomes, outcome{c.ref, false, "non-fast-forward"})
			continue
		}
		r.Refs[c.ref] = c.new
		outcomes = append(outcomes, outcome{c.ref, true, ""})
	}
	return s.pushReport(r, commands, outcomes, "")
}

type pushCmd struct {
	old, new, ref string
}

type outcome struct {
	ref    string
	ok     bool
	reason string
}

// pushReport renders the report-status reply: each report line is a
// pkt-line, carried inside a sideband band-1 packet; the stream ends
// with a flush.
func (s *server) pushReport(r *Repo, commands []pushCmd, outcomes []outcome, unpackErr string) []byte {
	var text bytes.Buffer
	if unpackErr != "" {
		text.WriteString("unpack " + unpackErr + "\n")
	} else {
		text.WriteString("unpack ok\n")
	}
	for _, o := range outcomes {
		if o.ok {
			text.WriteString("ok " + o.ref + "\n")
		} else {
			text.WriteString("ng " + o.ref + " " + o.reason + "\n")
		}
	}
	var body []byte
	for _, line := range bytes.SplitAfter(text.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		pl := pktLine(string(line))
		payload := append([]byte{1}, pl...)
		body = append(body, pktLineBytes(payload)...)
	}
	// Real git servers frame the report's terminating flush inside band 1
	// as well: the reader expects a flush packet in the demuxed stream,
	// and treats a bare EOF as a hang-up.
	body = append(body, pktLineBytes([]byte{1, '0', '0', '0', '0'})...)
	body = append(body, pktFlush...)
	return gitResp("application/x-git-receive-pack-result", body)
}
