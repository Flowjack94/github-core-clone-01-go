// The in-memory store: repositories, git objects, issues, pull requests.
// One process, one trial: no persistence, no disk. A single RWMutex guards
// the whole store; the hot paths take the read lock (cached responses) or a
// brief write lock (issue creation, ref update).
package main

import (
	"encoding/base64"
	"strconv"
	"sync"
)

// The viewer: the token owner. The scenario is fixed by the contest seed
// (env/src/wire.ts SCENARIO_SEED 0x6a09e667f3bcc909n), so the viewer login
// is the same in every trial: adaiversen.
const viewerLogin = "adaiversen"
const viewerEmail = "adaiversen@example.invalid"
const viewerID = 1000

// Fixed timestamps: no wall clock anywhere in a response, so live and
// replay produce byte-identical state.
const fixedEpoch = 1700000000
const fixedISO = "2023-11-14T22:13:20Z"

// The identity every server-created commit carries.
const commitAuthor = "Conformance <conformance@make.fun>"

const zeroOID = "0000000000000000000000000000000000000000"

type Object struct {
	Type string // blob, tree, commit, tag
	Data []byte // raw content, without the "<type> <len>\0" header
}

type Comment struct {
	ID   int
	Body string
}

type Issue struct {
	Number     int
	Title      string
	Body       string
	State      string // open | closed
	Labels     []string
	Comments   []*Comment
	cached     []byte // prebuilt GET-one response bytes (status line + headers + body)
	cachedList []byte // prebuilt list-entry JSON (used by list responses)
}

type PRFile struct {
	Filename  string
	Status    string // added | modified
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	Number   int
	Title    string
	Body     string
	Head     string // branch name
	Base     string // branch name
	HeadSHA  string
	BaseSHA  string
	State    string // open | closed
	Merged   bool
	MergedAt string
	MergeSHA string
	Files    []PRFile
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string // full ref name -> oid
	Branches      []string          // branch short names, creation order
	Objects       map[string]*Object
	Issues        map[int]*Issue
	IssueOrder    []int
	Pulls         map[int]*Pull
	PullOrder     []int
	NextNum       int // shared issue/PR number counter
	Labels        map[string]bool
	LabelOrder    []string
	NextComment   int
	Seq           int // creation sequence (user/repos listing order)
}

type Store struct {
	mu          sync.RWMutex
	repos       map[string]*Repo // owner + "/" + name
	repoList    []*Repo
	nextRepoID  int
	reqCounter  uint64
	issueReads  uint64
	issueWrites uint64
}

func newStore() *Store {
	return &Store{repos: make(map[string]*Repo), nextRepoID: 1}
}

func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

// newRepo creates and registers a repository. The caller holds the store
// write lock.
func (s *Store) newRepo(owner, name, description string, private bool) *Repo {
	r := &Repo{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		Refs:          make(map[string]string),
		Objects:       make(map[string]*Object),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		NextNum:       1,
		Labels:        make(map[string]bool),
		NextComment:   1,
		Seq:           len(s.repoList),
	}
	s.nextRepoID++
	s.repos[owner+"/"+name] = r
	s.repoList = append(s.repoList, r)
	return r
}

// repoID returns the stable numeric id for a repo (0 when absent).
func (s *Store) repoGlobalID(r *Repo) string {
	return "R" + strconv.Itoa(r.ID)
}

func b64opaque(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func repoGlobalID(r *Repo) string  { return b64opaque("Repository:" + strconv.Itoa(r.ID)) }
func issueGlobalID(r *Repo, n int) string {
	return b64opaque("Issue:" + strconv.Itoa(r.ID) + ":" + strconv.Itoa(n))
}
func pullGlobalID(r *Repo, n int) string {
	return b64opaque("PullRequest:" + strconv.Itoa(r.ID) + ":" + strconv.Itoa(n))
}
func commentGlobalID(r *Repo, n int) string {
	return b64opaque("Comment:" + strconv.Itoa(r.ID) + ":" + strconv.Itoa(n))
}

func (r *Repo) headRef() string { return "refs/heads/" + r.DefaultBranch }

func (r *Repo) headOID() string { return r.Refs[r.headRef()] }

// defaultBranchOID is the tip of the default branch, or "" when unborn.
func (r *Repo) defaultBranchOID() string { return r.Refs["refs/heads/"+r.DefaultBranch] }

func (r *Repo) refOID(ref string) (string, bool) {
	oid, ok := r.Refs[ref]
	return oid, ok
}
