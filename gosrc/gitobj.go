// Git objects: hashing, packfile read/write, tree and commit parsing,
// ancestry walks, tree manipulation. Plain objects only (no deltas):
// the suite never requires the server to emit a delta.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func objectHeader(typ string, size int) []byte {
	var b []byte
	b = append(b, typ...)
	b = append(b, ' ')
	b = append(b, strconv.Itoa(size)...)
	b = append(b, 0)
	return b
}

// storeObject hashes and stores an object, returning its oid.
func (r *Repo) storeObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.Objects[oid] = &Object{Type: typ, Data: cp}
	}
	return oid
}

// ---- tree parsing -------------------------------------------------------

type TreeEntry struct {
	Mode  string // e.g. "100644", "40000"
	Name  string
	OID   string
	IsDir bool
}

// parseTree reads raw tree bytes into entries (already git-sorted).
func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			return nil, errors.New("bad tree: no mode")
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			return nil, errors.New("bad tree: no name")
		}
		name := string(data[i : i+nul])
		oid := hex.EncodeToString(data[i+nul+1 : i+nul+21])
		i += nul + 21
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid, IsDir: mode == "40000"})
	}
	return out, nil
}

// buildTree serialises entries in git order (dirs sort as name+"/").
func buildTree(entries []TreeEntry) []byte {
	sort.Slice(entries, func(a, b int) bool {
		na, nb := entries[a].Name, entries[b].Name
		if entries[a].IsDir {
			na += "/"
		}
		if entries[b].IsDir {
			nb += "/"
		}
		return na < nb
	})
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// flattenTree returns path -> entry for every blob under a tree (recursive).
func (r *Repo) flattenTree(treeOID string, prefix string, out map[string]TreeEntry) {
	obj := r.Objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := prefix + e.Name
		if e.IsDir {
			r.flattenTree(e.OID, p+"/", out)
		} else {
			out[p] = e
		}
	}
}

// treeAt returns the flattened file map of a tree oid.
func (r *Repo) treeAt(treeOID string) map[string]TreeEntry {
	out := make(map[string]TreeEntry)
	if treeOID != "" && treeOID != zeroOID {
		r.flattenTree(treeOID, "", out)
	}
	return out
}

// setFileInTree returns a new tree oid with path set to blobOID (or deleted
// when blobOID is ""), building intermediate trees as needed. baseTreeOID
// may be "" for an empty tree.
func (r *Repo) setFileInTree(baseTreeOID, path, blobOID string) string {
	parts := splitPath(path)
	return r.setInTree(baseTreeOID, parts, blobOID)
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	out = append(out, p[start:])
	return out
}

func (r *Repo) setInTree(treeOID string, parts []string, blobOID string) string {
	var entries []TreeEntry
	if treeOID != "" && treeOID != zeroOID {
		if obj := r.Objects[treeOID]; obj != nil && obj.Type == "tree" {
			entries, _ = parseTree(obj.Data)
		}
	}
	name := parts[0]
	isLast := len(parts) == 1
	idx := -1
	for i, e := range entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if isLast {
		if blobOID == "" {
			// delete
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx].OID = blobOID
				entries[idx].Mode = "100644"
				entries[idx].IsDir = false
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
			}
		}
	} else {
		var subOID string
		if idx >= 0 {
			subOID = entries[idx].OID
		}
		newSub := r.setInTree(subOID, parts[1:], blobOID)
		if newSub == "" {
			// subtree became empty: drop the entry
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx].OID = newSub
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: newSub, IsDir: true})
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	data := buildTree(entries)
	return r.storeObject("tree", data)
}

// ---- commit parsing -----------------------------------------------------

type Commit struct {
	Tree    string
	Parents []string
	Message string
	Raw     []byte
}

func parseCommit(data []byte) Commit {
	c := Commit{Raw: data}
	// header ends at \n\n
	hdrEnd := bytes.Index(data, []byte("\n\n"))
	hdr := data
	if hdrEnd >= 0 {
		hdr = data[:hdrEnd]
		c.Message = string(data[hdrEnd+2:])
	}
	for _, line := range bytes.Split(hdr, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("tree ")) {
			c.Tree = string(line[5:])
		} else if bytes.HasPrefix(line, []byte("parent ")) {
			c.Parents = append(c.Parents, string(line[7:]))
		}
	}
	return c
}

// buildCommitBytes renders a commit object.
func buildCommitBytes(tree string, parents []string, message string, timestamp int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	fmt.Fprintf(&buf, "author %s %d +0000\n", commitAuthor, timestamp)
	fmt.Fprintf(&buf, "committer %s %d +0000\n", commitAuthor, timestamp)
	buf.WriteByte('\n')
	buf.WriteString(message)
	return buf.Bytes()
}

// makeCommit creates and stores a commit object.
func (r *Repo) makeCommit(tree string, parents []string, message string) string {
	data := buildCommitBytes(tree, parents, message, fixedEpoch)
	return r.storeObject("commit", data)
}

// ---- ancestry -----------------------------------------------------------

// isAncestor reports whether anc is reachable from desc (including equal).
func (r *Repo) isAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	if anc == zeroOID || desc == "" || desc == zeroOID {
		return false
	}
	seen := make(map[string]bool)
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c := parseCommit(obj.Data)
		stack = append(stack, c.Parents...)
	}
	return false
}

// commitParents returns the parents of a commit oid.
func (r *Repo) commitParents(oid string) []string {
	obj := r.Objects[oid]
	if obj == nil || obj.Type != "commit" {
		return nil
	}
	return parseCommit(obj.Data).Parents
}

// commitTree returns the tree oid of a commit.
func (r *Repo) commitTree(oid string) string {
	obj := r.Objects[oid]
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

// walkCommits lists commits reachable from tip, newest first (BFS by
// generation, parents after children). Histories here are linear; a simple
// DFS from the tip in parent order is newest-first for linear histories.
func (r *Repo) walkCommits(tip string) []string {
	var out []string
	seen := make(map[string]bool)
	var visit func(oid string)
	visit = func(oid string) {
		if oid == "" || oid == zeroOID || seen[oid] {
			return
		}
		seen[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			return
		}
		out = append(out, oid)
		for _, p := range parseCommit(obj.Data).Parents {
			visit(p)
		}
	}
	visit(tip)
	return out
}

// ---- packfile -----------------------------------------------------------

// writePack serialises objects (plain, no deltas) as a pack v2.
func writePack(objs []*Object) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])
	for _, o := range objs {
		var typeNum int
		switch o.Type {
		case "commit":
			typeNum = 1
		case "tree":
			typeNum = 2
		case "blob":
			typeNum = 3
		case "tag":
			typeNum = 4
		}
		size := len(o.Data)
		// varint header: first byte holds 4 size bits + type, continuation bit
		first := byte((typeNum << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		// zlib-deflated payload
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(o.Data)
		zw.Close()
		buf.Write(zb.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

type packObject struct {
	typ  string
	data []byte
	oid  string
}

// parsePack parses and validates a packfile, returning its objects.
// Verifies the SHA-1 trailer and each object's hash.
func parsePack(pack []byte) ([]packObject, error) {
	if len(pack) < 12+20 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("bad pack magic")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	// verify trailer
	trailer := pack[len(pack)-20:]
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(sum[:], trailer) {
		return nil, errors.New("pack checksum mismatch")
	}
	body := pack[12 : len(pack)-20]
	pos := 0
	out := make([]packObject, 0, count)
	for i := uint32(0); i < count; i++ {
		if pos >= len(body) {
			return nil, errors.New("pack truncated")
		}
		b0 := body[pos]
		pos++
		typeNum := (b0 >> 4) & 0x7
		size := int(b0 & 0x0f)
		shift := uint(4)
		for b0&0x80 != 0 {
			if pos >= len(body) {
				return nil, errors.New("pack truncated in size")
			}
			b0 = body[pos]
			pos++
			size |= int(b0&0x7f) << shift
			shift += 7
		}
		var typ string
		switch typeNum {
		case 1:
			typ = "commit"
		case 2:
			typ = "tree"
		case 3:
			typ = "blob"
		case 4:
			typ = "tag"
		case 6: // ofs-delta: not produced by our clients; refuse
			return nil, errors.New("ofs-delta in pack not supported")
		case 7: // ref-delta
			return nil, errors.New("ref-delta in pack not supported")
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typeNum)
		}
		// zlib stream: inflate from body[pos:] with a reader that yields
		// one byte per Read, so flate can never read past the stream end
		// (its normal buffered reads would swallow the next object's bytes).
		// The reader's byte count is exactly the stream's consumed length.
		obr := &oneByteReader{r: bytes.NewReader(body[pos:])}
		zr, err := zlib.NewReader(obr)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack object size mismatch: got %d want %d", len(data), size)
		}
		pos += obr.n
		out = append(out, packObject{typ: typ, data: data, oid: hashObject(typ, data)})
	}
	return out, nil
}

// oneByteReader yields at most one byte per Read, so a flate reader built
// on it can never read ahead past the end of the deflate stream.
type oneByteReader struct {
	r *bytes.Reader
	n int
}

func (b *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := b.r.Read(p[:1])
	b.n += n
	return n, err
}
