// REST v3 (/api/v3): repositories, branches, commits, contents, issues,
// comments, labels, pull requests, merges. Exact statuses and shapes from
// the suite's checks-rest.ts; every reply is application/json with a
// non-empty X-GitHub-Request-Id.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// ---- small JSON builders (hand-rolled: no reflection, fixed field order) --

type jb struct{ b []byte }

func (j *jb) str(s string) {
	j.b = append(j.b, strconv.Quote(s)...)
}
func (j *jb) raw(s string) { j.b = append(j.b, s...) }
func (j *jb) num(n int)    { j.b = strconv.AppendInt(j.b, int64(n), 10) }
func (j *jb) boolean(v bool) {
	if v {
		j.b = append(j.b, "true"...)
	} else {
		j.b = append(j.b, "false"...)
	}
}
func (j *jb) null() { j.b = append(j.b, "null"...) }

// repoJSON renders the repository object.
func repoJSON(r *Repo) []byte {
	j := &jb{}
	j.raw(`{"id":`)
	j.num(r.ID)
	j.raw(`,"name":`)
	j.str(r.Name)
	j.raw(`,"full_name":`)
	j.str(r.Owner + "/" + r.Name)
	j.raw(`,"owner":{"login":`)
	j.str(r.Owner)
	j.raw(`,"id":`)
	j.num(viewerID)
	j.raw(`,"type":"User"}`)
	j.raw(`,"private":`)
	j.boolean(r.Private)
	j.raw(`,"description":`)
	if r.Description == "" {
		j.null()
	} else {
		j.str(r.Description)
	}
	j.raw(`,"default_branch":`)
	j.str(r.DefaultBranch)
	j.raw(`,"has_issues":true,"has_pull_requests":true,"created_at":"`)
	j.raw(fixedISO)
	j.raw(`","updated_at":"`)
	j.raw(fixedISO)
	j.raw(`","html_url":`)
	j.str("https://github.com/" + r.Owner + "/" + r.Name)
	j.raw(`,"url":`)
	j.str("/api/v3/repos/" + r.Owner + "/" + r.Name)
	j.raw(`}`)
	return j.b
}

// issueJSON renders one issue object (labels included).
func issueJSON(r *Repo, iss *Issue) []byte {
	j := &jb{}
	j.raw(`{"id":`)
	j.num(r.ID*1000000 + iss.Number)
	j.raw(`,"number":`)
	j.num(iss.Number)
	j.raw(`,"title":`)
	j.str(iss.Title)
	j.raw(`,"body":`)
	j.str(iss.Body)
	j.raw(`,"state":`)
	j.str(iss.State)
	j.raw(`,"labels":[`)
	for i, l := range iss.Labels {
		if i > 0 {
			j.raw(",")
		}
		j.raw(`{"name":`)
		j.str(l)
		j.raw(`,"color":"ededed"}`)
	}
	j.raw(`],"user":{"login":`)
	j.str(viewerLogin)
	j.raw(`,"id":`)
	j.num(viewerID)
	j.raw(`},"created_at":"`)
	j.raw(fixedISO)
	j.raw(`","updated_at":"`)
	j.raw(fixedISO)
	j.raw(`","html_url":`)
	j.str("https://github.com/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(iss.Number))
	j.raw(`}`)
	return j.b
}

// pullJSON renders one pull request object.
func pullJSON(r *Repo, p *Pull) []byte {
	j := &jb{}
	j.raw(`{"id":`)
	j.num(r.ID*1000000 + p.Number)
	j.raw(`,"number":`)
	j.num(p.Number)
	j.raw(`,"title":`)
	j.str(p.Title)
	j.raw(`,"body":`)
	j.str(p.Body)
	j.raw(`,"state":`)
	j.str(p.State)
	j.raw(`,"head":{"ref":`)
	j.str(p.Head)
	j.raw(`,"sha":`)
	j.str(p.HeadSHA)
	j.raw(`,"label":`)
	j.str(r.Owner + ":" + p.Head)
	j.raw(`},"base":{"ref":`)
	j.str(p.Base)
	j.raw(`,"sha":`)
	j.str(p.BaseSHA)
	j.raw(`,"label":`)
	j.str(r.Owner + ":" + p.Base)
	j.raw(`},"merged":`)
	j.boolean(p.Merged)
	j.raw(`,"merged_at":`)
	if p.Merged {
		j.str(p.MergedAt)
	} else {
		j.null()
	}
	j.raw(`,"merge_commit_sha":`)
	if p.Merged {
		j.str(p.MergeSHA)
	} else {
		j.null()
	}
	j.raw(`,"user":{"login":`)
	j.str(viewerLogin)
	j.raw(`},"created_at":"`)
	j.raw(fixedISO)
	j.raw(`","updated_at":"`)
	j.raw(fixedISO)
	j.raw(`","html_url":`)
	j.str("https://github.com/" + r.Owner + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number))
	j.raw(`}`)
	return j.b
}

// branchJSON renders one branch entry.
func branchJSON(name, sha string) []byte {
	j := &jb{}
	j.raw(`{"name":`)
	j.str(name)
	j.raw(`,"commit":{"sha":`)
	j.str(sha)
	j.raw(`},"protected":false}`)
	return j.b
}

// commitJSON renders one commit entry.
func commitJSON(sha, message string) []byte {
	j := &jb{}
	j.raw(`{"sha":`)
	j.str(sha)
	j.raw(`,"commit":{"message":`)
	j.str(message)
	j.raw(`,"author":{"name":"Conformance","email":"conformance@make.fun","date":"`)
	j.raw(fixedISO)
	j.raw(`"},"committer":{"name":"Conformance","email":"conformance@make.fun","date":"`)
	j.raw(fixedISO)
	j.raw(`"}},"author":{"login":`)
	j.str(viewerLogin)
	j.raw(`}}`)
	return j.b
}

// ---- routing ------------------------------------------------------------

// routeREST handles /api/v3/<rest...>.
func (s *server) routeREST(req *request, rest []string) []byte {
	// Root and meta are public (GitHub semantics); everything else needs auth.
	if len(rest) == 0 {
		if req.method != "GET" {
			return errResp(404, "Not Found")
		}
		return jsonResp(200, []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`), "x-oauth-scopes", "repo, read:org")
	}
	if len(rest) == 1 && rest[0] == "meta" {
		if req.method != "GET" {
			return errResp(404, "Not Found")
		}
		return jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
	}
	if b := s.requireAuth(req); b != nil {
		return b
	}

	// GET /user
	if len(rest) == 1 && rest[0] == "user" {
		if req.method == "GET" {
			return jsonResp(200, []byte(`{"login":"`+viewerLogin+`","id":`+strconv.Itoa(viewerID)+`,"type":"User","name":"ada iversen","email":"`+viewerEmail+`"}`))
		}
		if req.method == "POST" {
			return s.restNotImplemented()
		}
		return errResp(404, "Not Found")
	}

	// /user/repos
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if req.method == "POST" {
			return s.createRepo(req)
		}
		if req.method == "GET" {
			return s.listUserRepos(req)
		}
		return errResp(404, "Not Found")
	}

	// /repos/{owner}/{repo}[...]
	if len(rest) >= 3 && rest[0] == "repos" {
		return s.routeRepo(req, rest[1], rest[2], rest[3:])
	}
	return errResp(404, "Not Found")
}

func (s *server) restNotImplemented() []byte { return errResp(404, "Not Found") }

// mustJSON parses a JSON body; on failure answers 400.
func mustJSON(req *request, v interface{}) []byte {
	if len(req.body) == 0 {
		return errResp(400, "request body must be valid JSON")
	}
	if err := json.Unmarshal(req.body, v); err != nil {
		return errResp(400, "request body is not valid JSON")
	}
	return nil
}

// lookupRepo resolves owner/name under the read lock; nil when the owner
// is not the viewer or the repo does not exist (both are 404).
func (s *server) lookupRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.store.getRepo(owner, name)
}

// createRepo implements POST /user/repos.
func (s *server) createRepo(req *request) []byte {
	var body struct {
		Name        string `json:"name"`
		Private     bool   `json:"private"`
		Description string `json:"description"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	if body.Name == "" || len(body.Name) > 100 {
		return errResp(422, "name is required")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.getRepo(viewerLogin, body.Name) != nil {
		return errResp(422, "Repository already exists")
	}
	r := s.store.newRepo(viewerLogin, body.Name, body.Description, body.Private)
	return jsonResp(201, repoJSON(r))
}

// listUserRepos implements GET /user/repos with Link pagination.
func (s *server) listUserRepos(req *request) []byte {
	perPage := pageSize(req, 30)
	page := pageNum(req, 1)
	s.store.mu.RLock()
	list := append([]*Repo(nil), s.store.repoList...)
	s.store.mu.RUnlock()
	total := len(list)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var b []byte
	b = append(b, '[')
	for i := start; i < end; i++ {
		if i > start {
			b = append(b, ',')
		}
		b = append(b, repoJSON(list[i])...)
	}
	b = append(b, ']')
	var extra []string
	if end < total {
		nextURL := s.pageURL(req, "/api/v3/user/repos", perPage, page+1)
		lastPage := (total + perPage - 1) / perPage
		lastURL := s.pageURL(req, "/api/v3/user/repos", perPage, lastPage)
		extra = []string{"link", `<` + nextURL + `>; rel="next", <` + lastURL + `>; rel="last"`}
	}
	return jsonResp(200, b, extra...)
}

// pageURL builds an absolute pagination URL from the Host header.
func (s *server) pageURL(req *request, path string, perPage, page int) string {
	host := req.header("host")
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + host + path + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
}

func pageSize(req *request, def int) int {
	v := req.query("per_page")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > 100 {
		n = 100
	}
	return n
}

func pageNum(req *request, def int) int {
	v := req.query("page")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// paginate slices a list of byte-chunks by page/per_page.
func paginate[T any](items []T, perPage, page int) []T {
	start := (page - 1) * perPage
	if start >= len(items) {
		return nil
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// routeRepo handles /repos/{owner}/{repo}/...
func (s *server) routeRepo(req *request, owner, name string, tail []string) []byte {
	m := req.method
	// GET /repos/{o}/{r}
	if len(tail) == 0 {
		r := s.lookupRepo(owner, name)
		if r == nil {
			return errResp(404, "Not Found")
		}
		switch m {
		case "GET":
			s.store.mu.RLock()
			b := repoJSON(r)
			s.store.mu.RUnlock()
			return jsonResp(200, b)
		case "PATCH":
			var body struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if b := mustJSON(req, &body); b != nil {
				return b
			}
			s.store.mu.Lock()
			if body.Description != nil {
				r.Description = *body.Description
			}
			if body.Private != nil {
				r.Private = *body.Private
			}
			b := repoJSON(r)
			s.store.mu.Unlock()
			return jsonResp(200, b)
		case "DELETE":
			s.store.mu.Lock()
			delete(s.store.repos, owner+"/"+name)
			for i, rr := range s.store.repoList {
				if rr == r {
					s.store.repoList = append(s.store.repoList[:i], s.store.repoList[i+1:]...)
					break
				}
			}
			s.store.mu.Unlock()
			return newResp(204, jsonType, nil)
		}
		return errResp(404, "Not Found")
	}

	r := s.lookupRepo(owner, name)
	if r == nil {
		return errResp(404, "Not Found")
	}
	switch tail[0] {
	case "branches":
		return s.routeBranches(req, r, tail[1:])
	case "commits":
		return s.routeCommits(req, r, tail[1:])
	case "contents":
		return s.routeContents(req, r, tail[1:])
	case "git":
		return s.routeGitData(req, r, tail[1:])
	case "labels":
		return s.routeLabels(req, r, tail[1:])
	case "issues":
		return s.routeIssues(req, r, tail[1:])
	case "pulls":
		return s.routePulls(req, r, tail[1:])
	}
	return errResp(404, "Not Found")
}

// routeBranches: GET /branches, GET /branches/{branch}.
func (s *server) routeBranches(req *request, r *Repo, tail []string) []byte {
	if req.method != "GET" {
		return errResp(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	if len(tail) == 0 {
		var b []byte
		b = append(b, '[')
		for i, br := range r.Branches {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, branchJSON(br, r.Refs["refs/heads/"+br])...)
		}
		b = append(b, ']')
		return jsonResp(200, b)
	}
	if len(tail) == 1 {
		oid, ok := r.Refs["refs/heads/"+tail[0]]
		if !ok {
			return errResp(404, "Not Found")
		}
		return jsonResp(200, branchJSON(tail[0], oid))
	}
	return errResp(404, "Not Found")
}

// routeCommits: GET /commits[?sha=<ref>].
func (s *server) routeCommits(req *request, r *Repo, tail []string) []byte {
	if req.method != "GET" || len(tail) != 0 {
		return errResp(404, "Not Found")
	}
	ref := req.query("sha")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var tip string
	if ref == "" {
		tip = r.defaultBranchOID()
	} else {
		// ref may be a branch name, a full ref, or an oid
		if oid, ok := r.Refs["refs/heads/"+ref]; ok {
			tip = oid
		} else if oid, ok := r.Refs[ref]; ok {
			tip = oid
		} else if _, ok := r.Objects[ref]; ok {
			tip = ref
		} else {
			return errResp(404, "Not Found")
		}
	}
	commits := r.walkCommits(tip)
	var b []byte
	b = append(b, '[')
	for i, oid := range commits {
		if i > 0 {
			b = append(b, ',')
		}
		msg := ""
		if obj := r.Objects[oid]; obj != nil {
			msg = parseCommit(obj.Data).Message
		}
		b = append(b, commitJSON(oid, msg)...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

// routeGitData: POST /git/refs, GET /git/refs/heads/{branch}.
func (s *server) routeGitData(req *request, r *Repo, tail []string) []byte {
	if len(tail) == 0 || tail[0] != "refs" {
		return errResp(404, "Not Found")
	}
	if req.method == "POST" && len(tail) == 1 {
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		if b := mustJSON(req, &body); b != nil {
			return b
		}
		if body.Ref == "" || body.SHA == "" {
			return errResp(422, "ref and sha are required")
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		if _, exists := r.Refs[body.Ref]; exists {
			return errResp(422, "Reference already exists")
		}
		if _, ok := r.Objects[body.SHA]; !ok {
			return errResp(422, "Object does not exist")
		}
		r.Refs[body.Ref] = body.SHA
		if strings.HasPrefix(body.Ref, "refs/heads/") {
			r.Branches = append(r.Branches, strings.TrimPrefix(body.Ref, "refs/heads/"))
		}
		j := &jb{}
		j.raw(`{"ref":`)
		j.str(body.Ref)
		j.raw(`,"object":{"sha":`)
		j.str(body.SHA)
		j.raw(`,"type":"commit"},"url":`)
		j.str("/api/v3/repos/" + r.Owner + "/" + r.Name + "/git/refs/" + body.Ref)
		j.raw(`}`)
		return jsonResp(201, j.b)
	}
	if req.method == "GET" && len(tail) == 3 && tail[1] == "heads" {
		s.store.mu.RLock()
		oid, ok := r.Refs["refs/heads/"+tail[2]]
		s.store.mu.RUnlock()
		if !ok {
			return errResp(404, "Not Found")
		}
		j := &jb{}
		j.raw(`{"ref":`)
		j.str("refs/heads/" + tail[2])
		j.raw(`,"object":{"sha":`)
		j.str(oid)
		j.raw(`,"type":"commit"}}`)
		return jsonResp(200, j.b)
	}
	return errResp(404, "Not Found")
}

// routeLabels: POST /labels.
func (s *server) routeLabels(req *request, r *Repo, tail []string) []byte {
	if req.method != "POST" || len(tail) != 0 {
		return errResp(404, "Not Found")
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	if body.Name == "" {
		return errResp(422, "name is required")
	}
	if body.Color == "" {
		body.Color = "ededed"
	}
	s.store.mu.Lock()
	if !r.Labels[body.Name] {
		r.Labels[body.Name] = true
		r.LabelOrder = append(r.LabelOrder, body.Name)
	}
	s.store.mu.Unlock()
	j := &jb{}
	j.raw(`{"name":`)
	j.str(body.Name)
	j.raw(`,"color":`)
	j.str(body.Color)
	j.raw(`}`)
	return jsonResp(201, j.b)
}

// ---- contents -----------------------------------------------------------

// resolveRef turns a ref query (branch, full ref, oid, "") into a commit oid.
func (r *Repo) resolveRef(ref string) string {
	if ref == "" {
		return r.defaultBranchOID()
	}
	if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		return oid
	}
	if oid, ok := r.Refs[ref]; ok {
		return oid
	}
	if _, ok := r.Objects[ref]; ok && r.Objects[ref].Type == "commit" {
		return ref
	}
	return ""
}

// routeContents: PUT/GET/DELETE /contents/{path...}.
func (s *server) routeContents(req *request, r *Repo, tail []string) []byte {
	if len(tail) == 0 {
		return errResp(404, "Not Found")
	}
	path := strings.Join(tail, "/")
	switch req.method {
	case "GET":
		return s.getContents(req, r, path)
	case "PUT":
		return s.putContents(req, r, path)
	case "DELETE":
		return s.deleteContents(req, r, path)
	}
	return errResp(404, "Not Found")
}

func (s *server) getContents(req *request, r *Repo, path string) []byte {
	ref := req.query("ref")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	tip := r.resolveRef(ref)
	if tip == "" {
		return errResp(404, "Not Found")
	}
	treeOID := r.commitTree(tip)
	files := r.treeAt(treeOID)
	if ent, ok := files[path]; ok {
		obj := r.Objects[ent.OID]
		if obj == nil {
			return errResp(404, "Not Found")
		}
		j := &jb{}
		j.raw(`{"type":"file","encoding":"base64","content":`)
		j.str(base64.StdEncoding.EncodeToString(obj.Data))
		j.raw(`,"sha":`)
		j.str(ent.OID)
		j.raw(`,"size":`)
		j.num(len(obj.Data))
		j.raw(`,"name":`)
		j.str(baseName(path))
		j.raw(`,"path":`)
		j.str(path)
		j.raw(`}`)
		return jsonResp(200, j.b)
	}
	// directory listing: immediate children of path
	prefix := path + "/"
	seen := make(map[string]TreeEntry)
	seenDir := make(map[string]bool)
	for p, ent := range files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seenDir[rest[:i]] = true
		} else {
			seen[rest] = ent
		}
	}
	if len(seen) == 0 && len(seenDir) == 0 {
		return errResp(404, "Not Found")
	}
	// directory entries sorted by name; dir sha = subtree oid
	names := make([]string, 0, len(seen)+len(seenDir))
	for n := range seen {
		names = append(names, n)
	}
	for n := range seenDir {
		names = append(names, n)
	}
	sortStrings(names)
	var b []byte
	b = append(b, '[')
	for i, n := range names {
		if i > 0 {
			b = append(b, ',')
		}
		if ent, ok := seen[n]; ok {
			j := &jb{}
			j.raw(`{"type":"file","name":`)
			j.str(n)
			j.raw(`,"path":`)
			j.str(path + "/" + n)
			j.raw(`,"sha":`)
			j.str(ent.OID)
			j.raw(`,"size":`)
			j.num(len(r.Objects[ent.OID].Data))
			j.raw(`}`)
			b = append(b, j.b...)
		} else {
			// find the subtree oid for this dir
			dirOID := r.dirOID(treeOID, path+"/"+n)
			j := &jb{}
			j.raw(`{"type":"dir","name":`)
			j.str(n)
			j.raw(`,"path":`)
			j.str(path + "/" + n)
			j.raw(`,"sha":`)
			j.str(dirOID)
			j.raw(`}`)
			b = append(b, j.b...)
		}
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

// dirOID returns the tree oid of a directory path under a tree.
func (r *Repo) dirOID(treeOID, dirPath string) string {
	parts := splitPath(dirPath)
	cur := treeOID
	for _, part := range parts {
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "tree" {
			return zeroOID
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return zeroOID
		}
		found := ""
		for _, e := range entries {
			if e.Name == part && e.IsDir {
				found = e.OID
				break
			}
		}
		if found == "" {
			return zeroOID
		}
		cur = found
	}
	return cur
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// putContents implements PUT /contents/{path} (create or update).
func (s *server) putContents(req *request, r *Repo, path string) []byte {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	data, err := base64.StdEncoding.DecodeString(strings.Map(func(c rune) rune {
		if c == '\r' || c == '\n' || c == '\t' || c == ' ' {
			return -1
		}
		return c
	}, body.Content))
	if err != nil {
		return errResp(422, "content is not valid base64")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	branch := body.Branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, exists := r.Refs[refName]
	if !exists {
		// Lenient: create the branch from the default tip (or unborn).
		tip = r.defaultBranchOID()
	}
	var curTree string
	var curBlob string
	if tip != "" {
		curTree = r.commitTree(tip)
		if ent, ok := r.treeAt(curTree)[path]; ok {
			curBlob = ent.OID
		}
	}
	created := curBlob == ""
	if body.SHA != "" {
		if curBlob == "" {
			return errResp(404, "Not Found")
		}
		if !strings.EqualFold(body.SHA, curBlob) {
			return errResp(409, "sha does not match the current blob")
		}
	} else if !created {
		return errResp(422, "file already exists; send its sha to update it")
	}
	blobOID := r.storeObject("blob", data)
	newTree := r.setFileInTree(curTree, path, blobOID)
	parents := []string{}
	if tip != "" {
		parents = []string{tip}
	}
	commitOID := r.makeCommit(newTree, parents, body.Message)
	r.Refs[refName] = commitOID
	if !exists {
		r.Branches = append(r.Branches, branch)
	}
	status := 201
	if !created {
		status = 200
	}
	j := &jb{}
	j.raw(`{"content":{"name":`)
	j.str(baseName(path))
	j.raw(`,"path":`)
	j.str(path)
	j.raw(`,"sha":`)
	j.str(blobOID)
	j.raw(`,"size":`)
	j.num(len(data))
	j.raw(`,"encoding":"base64"},"commit":{"sha":`)
	j.str(commitOID)
	j.raw(`,"message":`)
	j.str(body.Message)
	j.raw(`}}`)
	return jsonResp(status, j.b)
}

// deleteContents implements DELETE /contents/{path}.
func (s *server) deleteContents(req *request, r *Repo, path string) []byte {
	var body struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if len(req.body) > 0 {
		if b := mustJSON(req, &body); b != nil {
			return b
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	branch := body.Branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	refName := "refs/heads/" + branch
	tip, exists := r.Refs[refName]
	if !exists || tip == "" {
		return errResp(404, "Not Found")
	}
	curTree := r.commitTree(tip)
	ent, ok := r.treeAt(curTree)[path]
	if !ok {
		return errResp(404, "Not Found")
	}
	if body.SHA != "" && !strings.EqualFold(body.SHA, ent.OID) {
		return errResp(409, "sha does not match the current blob")
	}
	newTree := r.setFileInTree(curTree, path, "")
	commitOID := r.makeCommit(newTree, []string{tip}, body.Message)
	r.Refs[refName] = commitOID
	j := &jb{}
	j.raw(`{"commit":{"sha":`)
	j.str(commitOID)
	j.raw(`}}`)
	return jsonResp(200, j.b)
}

// ---- issues -------------------------------------------------------------

func (s *server) routeIssues(req *request, r *Repo, tail []string) []byte {
	m := req.method
	if len(tail) == 0 {
		if m == "POST" {
			return s.createIssue(req, r)
		}
		if m == "GET" {
			return s.listIssues(req, r)
		}
		return errResp(404, "Not Found")
	}
	num := parseID(tail[0])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	if len(tail) == 1 {
		switch m {
		case "GET":
			return s.getIssue(req, r, num)
		case "PATCH":
			return s.patchIssue(req, r, num)
		}
		return errResp(404, "Not Found")
	}
	if len(tail) == 2 && tail[1] == "comments" {
		switch m {
		case "POST":
			return s.createComment(req, r, num)
		case "GET":
			return s.listComments(req, r, num)
		}
	}
	return errResp(404, "Not Found")
}

// newIssue creates an issue (shared issue/PR number counter). The caller
// holds the store write lock.
func (r *Repo) newIssue(title, body string, labels []string) *Issue {
	n := r.NextNum
	r.NextNum++
	iss := &Issue{Number: n, Title: title, Body: body, State: "open", Labels: append([]string(nil), labels...)}
	r.Issues[n] = iss
	r.IssueOrder = append(r.IssueOrder, n)
	return iss
}

// newPull creates a pull request. The caller holds the store write lock.
// On failure it returns an empty pull and an error message.
func (r *Repo) newPull(title, body, head, base string) (*Pull, string) {
	headSHA, ok := r.Refs["refs/heads/"+head]
	if !ok {
		return nil, "head branch does not exist"
	}
	baseSHA, ok := r.Refs["refs/heads/"+base]
	if !ok {
		return nil, "base branch does not exist"
	}
	for _, n := range r.PullOrder {
		p := r.Pulls[n]
		if p.Head == head && p.State == "open" {
			return nil, "a pull request for this head already exists"
		}
	}
	mb := r.mergeBase(headSHA, baseSHA)
	files := r.diffTrees(r.commitTree(mb), r.commitTree(headSHA))
	n := r.NextNum
	r.NextNum++
	p := &Pull{
		Number:  n,
		Title:   title,
		Body:    body,
		Head:    head,
		Base:    base,
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
		State:   "open",
		Files:   files,
	}
	r.Pulls[n] = p
	r.PullOrder = append(r.PullOrder, n)
	return p, ""
}

// prConflict reports whether merging p would conflict: a file changed in
// merge-base->head whose base-tip content also moved and differs from the
// head content (or was deleted), or a file head deleted that the base tip
// changed. The caller holds any store lock.
func (r *Repo) prConflict(p *Pull) bool {
	baseTip := r.Refs["refs/heads/"+p.Base]
	headTip := r.Refs["refs/heads/"+p.Head]
	if baseTip == "" || headTip == "" {
		return false
	}
	mb := r.mergeBase(headTip, baseTip)
	baseFiles := r.treeAt(r.commitTree(mb))
	headFiles := r.treeAt(r.commitTree(headTip))
	curFiles := r.treeAt(r.commitTree(baseTip))
	for path, he := range headFiles {
		be, inBase := baseFiles[path]
		if !inBase {
			continue // new file in head: no conflict
		}
		if be.OID == he.OID {
			continue // head did not change it
		}
		ce, inCur := curFiles[path]
		if inCur && ce.OID != be.OID && ce.OID != he.OID {
			return true
		}
		if !inCur {
			return true // base deleted a file head changed
		}
	}
	// Head deleted a file the base tip changed: also a conflict.
	for path, be := range baseFiles {
		if _, inHead := headFiles[path]; inHead {
			continue
		}
		if ce, inCur := curFiles[path]; inCur && ce.OID != be.OID {
			return true
		}
	}
	return false
}

// doMerge performs a squash merge of p. The caller holds the store write
// lock. It returns (mergeSHA, "") on success, or ("", reason) with reason
// "conflict", "closed" or "gone".
func (r *Repo) doMerge(p *Pull) (string, string) {
	if p.Merged {
		return p.MergeSHA, ""
	}
	if p.State != "open" {
		return "", "closed"
	}
	baseTip := r.Refs["refs/heads/"+p.Base]
	headTip := r.Refs["refs/heads/"+p.Head]
	if baseTip == "" || headTip == "" {
		return "", "gone"
	}
	if r.prConflict(p) {
		return "", "conflict"
	}
	headTree := r.commitTree(headTip)
	msg := "Merge pull request #" + strconv.Itoa(p.Number) + " from " + r.Owner + "/" + p.Head + "\n"
	mergeSHA := r.makeCommit(headTree, []string{baseTip}, msg)
	r.Refs["refs/heads/"+p.Base] = mergeSHA
	p.State = "closed"
	p.Merged = true
	p.MergedAt = fixedISO
	p.MergeSHA = mergeSHA
	return mergeSHA, ""
}

// addComment appends a comment to an issue. The caller holds the store
// write lock.
func (r *Repo) addComment(iss *Issue, body string) *Comment {
	id := r.NextComment
	r.NextComment++
	c := &Comment{ID: id, Body: body}
	iss.Comments = append(iss.Comments, c)
	return c
}

func (s *server) createIssue(req *request, r *Repo) []byte {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range body.Labels {
		if !r.Labels[l] {
			return errResp(422, "unknown label "+l)
		}
	}
	iss := r.newIssue(body.Title, body.Body, body.Labels)
	return jsonResp(201, issueJSON(r, iss))
}

// listIssues implements GET /issues with state/sort/direction/pagination.
func (s *server) listIssues(req *request, r *Repo) []byte {
	state := req.query("state")
	if state == "" {
		state = "open"
	}
	sortDir := req.query("direction")
	if sortDir == "" {
		sortDir = "desc"
	}
	perPage := pageSize(req, 30)
	page := pageNum(req, 1)
	s.store.mu.RLock()
	var nums []int
	for _, n := range r.IssueOrder {
		iss := r.Issues[n]
		if state == "all" || iss.State == state {
			nums = append(nums, n)
		}
	}
	// numbers ascend in creation order; desc reverses
	if sortDir == "desc" {
		for i, j := 0, len(nums)-1; i < j; i, j = i+1, j-1 {
			nums[i], nums[j] = nums[j], nums[i]
		}
	}
	pageNums := paginate(nums, perPage, page)
	var b []byte
	b = append(b, '[')
	for i, n := range pageNums {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, issueJSON(r, r.Issues[n])...)
	}
	b = append(b, ']')
	s.store.mu.RUnlock()
	return jsonResp(200, b)
}

// getIssue serves one issue, using the cached response body when valid.
// This is the read workload's hot path: one map lookup, one Write.
func (s *server) getIssue(req *request, r *Repo, num int) []byte {
	s.store.mu.RLock()
	iss := r.Issues[num]
	if iss == nil {
		s.store.mu.RUnlock()
		return errResp(404, "Not Found")
	}
	cached := iss.cached
	s.store.mu.RUnlock()
	if cached != nil {
		return cached
	}
	// Build the full response once, cache it, and serve it.
	s.store.mu.Lock()
	iss = r.Issues[num]
	if iss == nil {
		s.store.mu.Unlock()
		return errResp(404, "Not Found")
	}
	if iss.cached == nil {
		iss.cached = jsonResp(200, issueJSON(r, iss))
	}
	cached = iss.cached
	s.store.mu.Unlock()
	return cached
}

// invalidateIssueCache drops the cached GET-one response after a mutation.
func (iss *Issue) invalidate() { iss.cached = nil }

func (s *server) patchIssue(req *request, r *Repo, num int) []byte {
	var body struct {
		State *string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	iss := r.Issues[num]
	if iss == nil {
		return errResp(404, "Not Found")
	}
	if body.State != nil {
		iss.State = *body.State
	}
	if body.Title != nil {
		iss.Title = *body.Title
	}
	if body.Body != nil {
		iss.Body = *body.Body
	}
	iss.invalidate()
	return jsonResp(200, issueJSON(r, iss))
}

func (s *server) createComment(req *request, r *Repo, num int) []byte {
	var body struct {
		Body string `json:"body"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	iss := r.Issues[num]
	if iss == nil {
		return errResp(404, "Not Found")
	}
	c := r.addComment(iss, body.Body)
	j := &jb{}
	j.raw(`{"id":`)
	j.num(r.ID*10000000 + c.ID)
	j.raw(`,"body":`)
	j.str(body.Body)
	j.raw(`,"user":{"login":`)
	j.str(viewerLogin)
	j.raw(`},"created_at":"`)
	j.raw(fixedISO)
	j.raw(`"}`)
	return jsonResp(201, j.b)
}

func (s *server) listComments(req *request, r *Repo, num int) []byte {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	iss := r.Issues[num]
	if iss == nil {
		return errResp(404, "Not Found")
	}
	var b []byte
	b = append(b, '[')
	for i, c := range iss.Comments {
		if i > 0 {
			b = append(b, ',')
		}
		j := &jb{}
		j.raw(`{"id":`)
		j.num(r.ID*10000000 + c.ID)
		j.raw(`,"body":`)
		j.str(c.Body)
		j.raw(`,"user":{"login":`)
		j.str(viewerLogin)
		j.raw(`},"created_at":"`)
		j.raw(fixedISO)
		j.raw(`"}`)
		b = append(b, j.b...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

// ---- pulls --------------------------------------------------------------

func (s *server) routePulls(req *request, r *Repo, tail []string) []byte {
	m := req.method
	if len(tail) == 0 {
		if m == "POST" {
			return s.createPull(req, r)
		}
		if m == "GET" {
			return s.listPulls(req, r)
		}
		return errResp(404, "Not Found")
	}
	num := parseID(tail[0])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	if len(tail) == 1 {
		if m == "GET" {
			return s.getPull(req, r, num)
		}
		return errResp(404, "Not Found")
	}
	if len(tail) == 2 && tail[1] == "files" && m == "GET" {
		return s.pullFiles(req, r, num)
	}
	if len(tail) == 2 && tail[1] == "merge" && m == "PUT" {
		return s.mergePull(req, r, num)
	}
	return errResp(404, "Not Found")
}

// mergeBase finds the first common ancestor of a and b (linear histories).
func (r *Repo) mergeBase(a, b string) string {
	inB := make(map[string]bool)
	for _, c := range r.walkCommits(b) {
		inB[c] = true
	}
	for _, c := range r.walkCommits(a) {
		if inB[c] {
			return c
		}
	}
	return ""
}

// diffTrees computes changed files between baseTree and headTree.
func (r *Repo) diffTrees(baseTree, headTree string) []PRFile {
	base := r.treeAt(baseTree)
	head := r.treeAt(headTree)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sortStrings(names)
	var out []PRFile
	for _, p := range names {
		be, inBase := base[p]
		he, inHead := head[p]
		switch {
		case inHead && !inBase:
			data := r.Objects[he.OID].Data
			n := countLines(data)
			out = append(out, PRFile{Filename: p, Status: "added", Additions: n, Deletions: 0, Changes: n})
		case inBase && !inHead:
			data := r.Objects[be.OID].Data
			n := countLines(data)
			out = append(out, PRFile{Filename: p, Status: "removed", Additions: 0, Deletions: n, Changes: n})
		case be.OID != he.OID:
			add, del := diffLines(r.Objects[be.OID].Data, r.Objects[he.OID].Data)
			out = append(out, PRFile{Filename: p, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return out
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// diffLines counts added/removed lines by multiset difference.
func diffLines(oldData, newData []byte) (add, del int) {
	oldLines := splitLines(oldData)
	newLines := splitLines(newData)
	counts := make(map[string]int)
	for _, l := range oldLines {
		counts[l]++
	}
	for _, l := range newLines {
		if counts[l] > 0 {
			counts[l]--
		} else {
			add++
		}
	}
	for _, c := range counts {
		del += c
	}
	return add, del
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func (s *server) createPull(req *request, r *Repo) []byte {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if b := mustJSON(req, &body); b != nil {
		return b
	}
	if body.Head == "" || body.Base == "" {
		return errResp(422, "head and base are required")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	p, errMsg := r.newPull(body.Title, body.Body, body.Head, body.Base)
	if errMsg != "" {
		return errResp(422, errMsg)
	}
	return jsonResp(201, pullJSON(r, p))
}

func (s *server) listPulls(req *request, r *Repo) []byte {
	state := req.query("state")
	if state == "" {
		state = "open"
	}
	perPage := pageSize(req, 30)
	page := pageNum(req, 1)
	s.store.mu.RLock()
	var nums []int
	for _, n := range r.PullOrder {
		p := r.Pulls[n]
		if state == "all" || p.State == state {
			nums = append(nums, n)
		}
	}
	pageNums := paginate(nums, perPage, page)
	var b []byte
	b = append(b, '[')
	for i, n := range pageNums {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, pullJSON(r, r.Pulls[n])...)
	}
	b = append(b, ']')
	s.store.mu.RUnlock()
	return jsonResp(200, b)
}

func (s *server) getPull(req *request, r *Repo, num int) []byte {
	s.store.mu.RLock()
	p := r.Pulls[num]
	if p == nil {
		s.store.mu.RUnlock()
		return errResp(404, "Not Found")
	}
	b := pullJSON(r, p)
	s.store.mu.RUnlock()
	return jsonResp(200, b)
}

func (s *server) pullFiles(req *request, r *Repo, num int) []byte {
	s.store.mu.RLock()
	p := r.Pulls[num]
	if p == nil {
		s.store.mu.RUnlock()
		return errResp(404, "Not Found")
	}
	files := p.Files
	s.store.mu.RUnlock()
	var b []byte
	b = append(b, '[')
	for i, f := range files {
		if i > 0 {
			b = append(b, ',')
		}
		j := &jb{}
		j.raw(`{"filename":`)
		j.str(f.Filename)
		j.raw(`,"status":`)
		j.str(f.Status)
		j.raw(`,"additions":`)
		j.num(f.Additions)
		j.raw(`,"deletions":`)
		j.num(f.Deletions)
		j.raw(`,"changes":`)
		j.num(f.Changes)
		j.raw(`}`)
		b = append(b, j.b...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

// mergePull implements PUT /pulls/{n}/merge (squash).
func (s *server) mergePull(req *request, r *Repo, num int) []byte {
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if len(req.body) > 0 {
		// A malformed merge body is a 400 like any other JSON body.
		if b := mustJSON(req, &body); b != nil {
			return b
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	p := r.Pulls[num]
	if p == nil {
		return errResp(404, "Not Found")
	}
	mergeSHA, reason := r.doMerge(p)
	switch reason {
	case "conflict":
		return errResp(405, "Merge conflict")
	case "closed":
		return errResp(405, "Pull request is not open")
	case "gone":
		return errResp(422, "head or base branch is gone")
	}
	j := &jb{}
	j.raw(`{"sha":`)
	j.str(mergeSHA)
	j.raw(`,"merged":true,"message":"Pull request successfully merged"}`)
	return jsonResp(200, j.b)
}
