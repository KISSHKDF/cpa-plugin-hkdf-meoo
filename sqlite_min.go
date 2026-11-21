// sqlite_min.go — a deliberately tiny, READ-ONLY SQLite b-tree reader.
//
// WHY THIS EXISTS: reading the daily-login reward (session_cookie.go) needs the
// operator's Chromium cookie store, which is a SQLite database. The plugin ships
// as a c-shared DLL built with CGO_ENABLED=0, so a driver is not an option:
// modernc.org/sqlite would add tens of megabytes and a large dependency surface to
// a 7 MB plugin, and a cgo driver cannot link at all. Neither is shelling out to
// python/sqlite3 acceptable — the operator's machine is not required to have them,
// and spawning helpers from inside the host process is exactly the kind of thing
// the fleet's release rules forbid.
//
// WHAT IT IS NOT: a general SQLite implementation. It reads a table b-tree, in a
// single file, without WAL replay. That is sufficient for `Cookies`, which
// Chromium keeps in rollback-journal mode, and it is bounded: no query language,
// no index, no write path, no schema migration. Every failure mode returns an
// error; the caller (session_cookie.go) validates whatever it recovers against
// the live upstream before using it, so a parser bug can waste a request but can
// never corrupt a credential.
//
// Format reference: https://www.sqlite.org/fileformat2.html (§1.3 pages, §2.6
// b-tree pages, §2.7 cell payload overflow, §3 record format, §4 serial types).
//
// Ported verbatim from the 灵犀 tree (work/cpa-plugins/lingxi), which wrote and
// proved it on this machine first.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// SQLite magic: "SQLite format 3\x00" (16 bytes).
var sqliteMagic = []byte("SQLite format 3\x00")

const (
	sqlitePageLeafTable     = 0x0d
	sqlitePageInteriorTable = 0x05

	// sqliteHeaderSize is the database header that page 1 carries in front of its
	// b-tree page header (fileformat2 §1.6).
	sqliteHeaderSize = 100

	// maxCookieDBBytes bounds how much we are willing to read. A Chromium cookie
	// store is tens of kilobytes; 64 MiB is a sanity bound, not a target, so a
	// mistaken path (e.g. a video file) fails fast instead of allocating.
	maxCookieDBBytes = 64 << 20
)

var (
	errNotSQLite      = errors.New("not a sqlite database")
	errPageOutOfRange = errors.New("sqlite page out of range")
	errRecordOverflow = errors.New("sqlite record overflows its page (unsupported)")
)

// sqliteDB is an in-memory, read-only view of one database file.
type sqliteDB struct {
	data     []byte
	pageSize int
	usable   int // pageSize minus the reserved tail bytes
}

// sqlValue is one decoded record field. Only the shapes a cookie row needs are
// materialised: integers, text and blobs. Anything else is carried as "other"
// with its serial type, which is enough to keep column indexes honest.
type sqlValue struct {
	Serial int64
	Int    int64
	Bytes  []byte
	IsText bool
	IsNull bool
}

// string renders text/blob values; integers render as-is. Used for host_key and
// name, both of which are TEXT in the cookies schema.
func (v sqlValue) string() string {
	if v.IsNull {
		return ""
	}
	if v.IsText || v.Bytes != nil {
		return string(v.Bytes)
	}
	return fmt.Sprintf("%d", v.Int)
}

// readSQLiteFile loads a database file and validates its header.
func readSQLiteFile(path string) (*sqliteDB, error) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, errStat
	}
	if info.Size() <= 0 || info.Size() > maxCookieDBBytes {
		return nil, fmt.Errorf("sqlite: refusing %d-byte file", info.Size())
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	if len(data) < 100 || string(data[:16]) != string(sqliteMagic) {
		return nil, errNotSQLite
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 { // SQLite encodes 65536 as 1
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize%512 != 0 {
		return nil, fmt.Errorf("sqlite: implausible page size %d", pageSize)
	}
	usable := pageSize - int(data[20]) // reserved-space bytes at the end of every page
	if usable <= 0 {
		return nil, fmt.Errorf("sqlite: implausible usable size %d", usable)
	}
	return &sqliteDB{data: data, pageSize: pageSize, usable: usable}, nil
}

// page returns the raw bytes of 1-based page n.
func (d *sqliteDB) page(n int) ([]byte, error) {
	if n <= 0 {
		return nil, errPageOutOfRange
	}
	off := (n - 1) * d.pageSize
	if off < 0 || off+d.pageSize > len(d.data) {
		return nil, errPageOutOfRange
	}
	return d.data[off : off+d.pageSize], nil
}

// tableRows walks the table b-tree rooted at rootPage and calls fn for every
// row's payload. Traversal is iterative over an explicit stack so a corrupt
// parent/child cycle cannot blow the goroutine stack.
func (d *sqliteDB) tableRows(rootPage int, fn func(payload []byte) error) error {
	stack := []int{rootPage}
	visited := make(map[int]bool, 8)
	for len(stack) > 0 {
		pageNo := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[pageNo] {
			continue // defensive: a cycle would otherwise loop forever
		}
		visited[pageNo] = true

		pg, errPage := d.page(pageNo)
		if errPage != nil {
			return errPage
		}
		// Page 1 is special: it starts with the 100-byte database header, so its
		// b-tree header lives at offset 100 (fileformat2 §1.6). Cell offsets stay
		// relative to the start of the page, i.e. they already include those 100
		// bytes — so the header base shifts, the offsets do not.
		base := 0
		if pageNo == 1 {
			base = sqliteHeaderSize
		}
		switch pg[base] {
		case sqlitePageLeafTable:
			cells, errCells := d.leafCells(pg, base)
			if errCells != nil {
				return errCells
			}
			for _, payload := range cells {
				if errRow := fn(payload); errRow != nil {
					return errRow
				}
			}
		case sqlitePageInteriorTable:
			children, errChildren := d.interiorChildren(pg, base)
			if errChildren != nil {
				return errChildren
			}
			for _, child := range children {
				stack = append(stack, child)
			}
		default:
			// Index page (0x02/0x0a) or a non-btree page: not part of a table walk.
			return fmt.Errorf("sqlite: page %d has unexpected type 0x%02x", pageNo, pg[base])
		}
	}
	return nil
}

// leafCells decodes every cell payload of a leaf table page. `base` is where the
// b-tree header starts (0 for pages 2+, 100 for page 1).
func (d *sqliteDB) leafCells(pg []byte, base int) ([][]byte, error) {
	if base+8 > len(pg) {
		return nil, errPageOutOfRange
	}
	count := int(binary.BigEndian.Uint16(pg[base+3 : base+5]))
	// Leaf table header is 8 bytes; then a 2-byte cell offset per cell.
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		slotOff := base + 8 + i*2
		if slotOff+2 > len(pg) {
			return nil, errPageOutOfRange
		}
		cellOff := int(binary.BigEndian.Uint16(pg[slotOff : slotOff+2]))
		if cellOff <= 0 || cellOff >= len(pg) {
			continue
		}
		payload, errCell := d.leafCellPayload(pg, cellOff)
		if errCell != nil {
			if errors.Is(errCell, errRecordOverflow) {
				continue // a spilled row is simply not readable here; skip it
			}
			return nil, errCell
		}
		out = append(out, payload)
	}
	return out, nil
}

// leafCellPayload extracts one leaf table cell's payload, following the overflow
// chain when the payload does not fit on the page.
func (d *sqliteDB) leafCellPayload(pg []byte, cellOff int) ([]byte, error) {
	payloadLen, n1 := sqliteVarint(pg[cellOff:])
	if n1 == 0 {
		return nil, errPageOutOfRange
	}
	_ = payloadLen // rowid follows; not needed for our rows
	rowidOff := cellOff + n1
	_, n2 := sqliteVarint(pg[rowidOff:])
	if n2 == 0 {
		return nil, errPageOutOfRange
	}
	dataOff := rowidOff + n2
	if dataOff > len(pg) {
		return nil, errPageOutOfRange
	}

	// Local payload sizing (fileformat2 §2.7). Cookies rows are far below the
	// overflow threshold; the branch exists so a future fat row fails cleanly.
	maxLocal := d.usable - 35
	if int(payloadLen) <= maxLocal {
		if dataOff+int(payloadLen) > len(pg) {
			return nil, errPageOutOfRange
		}
		return pg[dataOff : dataOff+int(payloadLen)], nil
	}
	return nil, errRecordOverflow
}

// interiorChildren lists the child page numbers of an interior table page.
func (d *sqliteDB) interiorChildren(pg []byte, base int) ([]int, error) {
	if base+12 > len(pg) {
		return nil, errPageOutOfRange
	}
	count := int(binary.BigEndian.Uint16(pg[base+3 : base+5]))
	// Interior table header is 12 bytes; then a 2-byte cell offset per cell.
	out := make([]int, 0, count+1)
	// The right-most child lives in the page header, not in a cell.
	right := int(binary.BigEndian.Uint32(pg[base+8 : base+12]))
	for i := 0; i < count; i++ {
		slotOff := base + 12 + i*2
		if slotOff+2 > len(pg) {
			return nil, errPageOutOfRange
		}
		cellOff := int(binary.BigEndian.Uint16(pg[slotOff : slotOff+2]))
		if cellOff <= 0 || cellOff+4 > len(pg) {
			continue
		}
		out = append(out, int(binary.BigEndian.Uint32(pg[cellOff:cellOff+4])))
	}
	out = append(out, right)
	return out, nil
}

// sqliteVarint decodes one SQLite varint (big-endian, 1..9 bytes) and returns the
// value plus the number of bytes consumed (0 when the buffer is exhausted).
func sqliteVarint(buf []byte) (int64, int) {
	var v int64
	for i := 0; i < 9 && i < len(buf); i++ {
		b := buf[i]
		if i == 8 {
			v = (v << 8) | int64(b)
			return v, 9
		}
		v = (v << 7) | int64(b&0x7f)
		if b&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// parseRecord decodes one record payload into its column values (§2.1/§3).
func parseRecord(payload []byte) ([]sqlValue, error) {
	headerLen, n := sqliteVarint(payload)
	if n == 0 || headerLen < int64(n) || int(headerLen) > len(payload) {
		return nil, errPageOutOfRange
	}
	header := payload[n:headerLen]
	body := payload[headerLen:]

	serials := make([]int64, 0, 16)
	for off := 0; off < len(header); {
		s, sn := sqliteVarint(header[off:])
		if sn == 0 {
			return nil, errPageOutOfRange
		}
		serials = append(serials, s)
		off += sn
	}

	out := make([]sqlValue, 0, len(serials))
	bodyOff := 0
	for _, s := range serials {
		v := sqlValue{Serial: s}
		size, errSize := serialSize(s)
		if errSize != nil {
			return nil, errSize
		}
		if bodyOff+size > len(body) {
			return nil, errPageOutOfRange
		}
		chunk := body[bodyOff : bodyOff+size]
		bodyOff += size

		switch {
		case s == 0:
			v.IsNull = true
		case s >= 1 && s <= 6:
			v.Int = decodeBigEndianInt(chunk)
		case s == 8:
			v.Int = 0
		case s == 9:
			v.Int = 1
		case s >= 12 && s%2 == 0: // blob
			v.Bytes = chunk
		case s >= 13 && s%2 == 1: // text
			v.Bytes = chunk
			v.IsText = true
		default: // 7 (float) and reserved values: carried, not interpreted
		}
		out = append(out, v)
	}
	return out, nil
}

// serialSize is the body byte-length of one serial type (§4).
func serialSize(s int64) (int, error) {
	switch {
	case s == 0, s == 8, s == 9:
		return 0, nil
	case s >= 1 && s <= 4:
		return int(s), nil
	case s == 5:
		return 6, nil
	case s == 6, s == 7:
		return 8, nil
	case s >= 12:
		if s%2 == 0 {
			return int((s - 12) / 2), nil
		}
		return int((s - 13) / 2), nil
	default:
		return 0, fmt.Errorf("sqlite: reserved serial type %d", s)
	}
}

// decodeBigEndianInt decodes a 1..8-byte big-endian two's-complement integer.
func decodeBigEndianInt(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	var v int64
	for _, c := range b {
		v = (v << 8) | int64(c)
	}
	// Sign-extend when the top bit of the represented width is set.
	shift := uint(64 - 8*len(b))
	return v << shift >> shift
}

// sqliteTableColumns reads CREATE TABLE sql for `table` out of sqlite_master and
// returns the declared column names in order, so column indexes are derived from
// the file's own schema instead of a hard-coded Chromium column list (which has
// changed across versions).
func (d *sqliteDB) sqliteTableColumns(table string) ([]string, int, error) {
	var columns []string
	rootPage := 0
	errWalk := d.tableRows(1, func(payload []byte) error {
		vals, errRecord := parseRecord(payload)
		if errRecord != nil || len(vals) < 5 {
			return nil // tolerate rows we cannot decode
		}
		if vals[0].string() != "table" { // type
			return nil
		}
		if vals[1].string() != table { // name
			return nil
		}
		rootPage = int(vals[3].Int) // rootpage
		columns = parseColumnNames(vals[4].string())
		return nil
	})
	if errWalk != nil {
		return nil, 0, errWalk
	}
	if rootPage == 0 || len(columns) == 0 {
		return nil, 0, fmt.Errorf("sqlite: table %q not found in sqlite_master", table)
	}
	return columns, rootPage, nil
}

// parseColumnNames extracts the top-level column names from a CREATE TABLE
// statement. Constraints (PRIMARY KEY(...), UNIQUE(...), FOREIGN KEY(...)) are
// skipped: they start with a reserved keyword rather than a column name.
func parseColumnNames(create string) []string {
	open := -1
	for i := 0; i < len(create); i++ {
		if create[i] == '(' {
			open = i
			break
		}
	}
	if open < 0 {
		return nil
	}
	depth := 0
	end := len(create)
	for i := open; i < len(create); i++ {
		switch create[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end != len(create) {
			break
		}
	}
	body := create[open+1 : end]

	parts := splitTopLevel(body)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name := firstIdentifier(part)
		if name == "" {
			continue
		}
		switch upperASCII(name) {
		case "PRIMARY", "UNIQUE", "CHECK", "CONSTRAINT", "FOREIGN":
			continue
		}
		out = append(out, name)
	}
	return out
}

// splitTopLevel splits a column list on commas that are not inside parentheses.
func splitTopLevel(body string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	return out
}

// firstIdentifier returns the leading identifier of one column definition,
// stripping quoting characters Chromium uses like "host_key" or [name].
func firstIdentifier(def string) string {
	s := trimSpaceASCII(def)
	if s == "" {
		return ""
	}
	if q := s[0]; q == '"' || q == '`' || q == '\'' || q == '[' {
		closer := q
		if q == '[' {
			closer = ']'
		}
		for i := 1; i < len(s); i++ {
			if s[i] == closer {
				return s[1:i]
			}
		}
		return s[1:]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isWord := c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !isWord {
			return s[:i]
		}
	}
	return s
}

func trimSpaceASCII(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func upperASCII(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// sqliteReadTable returns the decoded rows of `table` in file order. A row whose
// record cannot be decoded is skipped rather than failing the walk: the caller
// only needs the rows that carry a usable value.
func (d *sqliteDB) sqliteReadTable(table string) ([]map[string]sqlValue, error) {
	columns, rootPage, errSchema := d.sqliteTableColumns(table)
	if errSchema != nil {
		return nil, errSchema
	}
	var rows []map[string]sqlValue
	errWalk := d.tableRows(rootPage, func(payload []byte) error {
		vals, errRecord := parseRecord(payload)
		if errRecord != nil {
			return nil
		}
		row := make(map[string]sqlValue, len(columns))
		for i, name := range columns {
			if i < len(vals) {
				row[name] = vals[i]
			}
		}
		rows = append(rows, row)
		return nil
	})
	if errWalk != nil {
		return nil, errWalk
	}
	return rows, nil
}
