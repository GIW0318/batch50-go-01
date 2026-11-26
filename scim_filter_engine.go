// Package task implements schema-aware SCIM filter evaluation, deterministic
// sorting, keyset pagination and recoverable engine snapshots.
package task

import (
	"bytes"
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	OpEq = "eq"
	OpNe = "ne"
	OpCo = "co"
	OpSw = "sw"
	OpEw = "ew"
	OpPr = "pr"
	OpGt = "gt"
	OpGe = "ge"
	OpLt = "lt"
	OpLe = "le"
)

const (
	boolAnd = "and"
	boolOr  = "or"
	boolNot = "not"
)

type ValueKind string

const (
	KindString ValueKind = "string"
	KindNumber ValueKind = "number"
	KindBool   ValueKind = "bool"
)

var (
	ErrBadFilter       = errors.New("filter expression is malformed")
	ErrUnknownAttr     = errors.New("attribute path is not registered")
	ErrDuplicateSchema = errors.New("attribute schema is already registered")
	ErrUnknownKind     = errors.New("resource kind is unknown")
	ErrDuplicateID     = errors.New("resource id already exists")
	ErrNotFound        = errors.New("resource not found")
	ErrCursorInvalid   = errors.New("cursor is malformed or was altered")
	ErrCursorMismatch  = errors.New("cursor was issued for another expression")
	ErrResultLimit     = errors.New("matched resources exceed the configured limit")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

type Value struct {
	Kind ValueKind
	Str  string
	Num  float64
	Bool bool
}

type AttrSchema struct {
	Path      string
	Sub       string
	CaseExact bool
	Kind      ValueKind
}

type AttrRef struct {
	Path string
	Sub  string
}

type Resource struct {
	ID    string
	Kind  string
	Attrs map[string][]Value
}

type Limits struct {
	MaxCacheEntries int
	MaxPageSize     int
	MaxResults      int
}

type QueryRequest struct {
	Filter     string
	SortBy     string
	Descending bool
	PageSize   int
	Cursor     string
}

type QueryPage struct {
	Resources   []Resource
	NextCursor  string
	Total       int
	Fingerprint string
}

type CacheStats struct {
	Entries int
	Hits    int
	Misses  int
}

type Expr struct {
	text        string
	fingerprint string
	root        exprNode
}

type exprNode interface{ filterNode() }

type binaryNode struct {
	op    string
	left  exprNode
	right exprNode
}

type notNode struct{ operand exprNode }

type compareNode struct {
	ref        AttrRef
	op         string
	literal    Value
	hasLiteral bool
	schema     AttrSchema
}

func (*binaryNode) filterNode()  {}
func (*notNode) filterNode()     {}
func (*compareNode) filterNode() {}

type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenIdent
	tokenString
	tokenNumber
	tokenBool
	tokenLParen
	tokenRParen
)

type token struct {
	kind  tokenKind
	text  string
	value Value
}

type filterParser struct {
	tokens []token
	pos    int
}

type compileCall struct {
	done chan struct{}
	expr Expr
	err  error
}

type cacheEntry struct {
	expr Expr
	raws map[string]struct{}
	elem *list.Element
}

type sortPlan struct {
	active     bool
	ref        AttrRef
	schema     AttrSchema
	descending bool
}

type sortTuple struct {
	present bool
	value   Value
	id      string
}

type storedCursor struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	Kind        string `json:"kind,omitempty"`
	Sort        string `json:"sort,omitempty"`
	Descending  bool   `json:"descending,omitempty"`
	ID          string `json:"id"`
	HasValue    bool   `json:"has_value"`
	Value       *Value `json:"value,omitempty"`
}

type tombstoneRecord struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
}

type cacheSnapshot struct {
	Filter      string `json:"filter"`
	Fingerprint string `json:"fingerprint"`
}

type snapshotData struct {
	Magic       string            `json:"magic"`
	Version     int               `json:"version"`
	Sequence    uint64            `json:"sequence"`
	Schemas     []AttrSchema      `json:"schemas"`
	Resources   []Resource        `json:"resources"`
	Tombstones  []tombstoneRecord `json:"tombstones"`
	Cache       []cacheSnapshot   `json:"cache,omitempty"`
	CacheHits   int               `json:"cache_hits"`
	CacheMisses int               `json:"cache_misses"`
}

type ScimFilterEngine struct {
	limits Limits

	stateMu    sync.RWMutex
	schemas    map[AttrRef]AttrSchema
	resources  map[string]map[string]Resource
	tombstones map[string]map[string]uint64
	sequence   uint64

	cacheMu   sync.RWMutex
	cache     map[string]*cacheEntry
	rawIndex  map[string]string
	inflight  map[string]*compileCall
	lru       *list.List
	cacheHits int
	cacheMiss int
}

const (
	snapshotMagic   = "gsb-batch50-go-safety-a-01-scim"
	snapshotVersion = 1
	cursorPrefix    = "v1"
	cursorVersion   = 1
)

var cursorHMACKey = []byte("scim-filter-engine-local-cursor-integrity-v1")

func NewScimFilterEngine(limits Limits) *ScimFilterEngine {
	if limits.MaxCacheEntries < 0 {
		limits.MaxCacheEntries = 0
	}
	if limits.MaxPageSize < 0 {
		limits.MaxPageSize = 0
	}
	if limits.MaxResults < 0 {
		limits.MaxResults = 0
	}
	resources := make(map[string]map[string]Resource)
	tombstones := make(map[string]map[string]uint64)
	for _, kind := range []string{"User", "Group"} {
		resources[kind] = make(map[string]Resource)
		tombstones[kind] = make(map[string]uint64)
	}
	return &ScimFilterEngine{
		limits:     limits,
		schemas:    make(map[AttrRef]AttrSchema),
		resources:  resources,
		tombstones: tombstones,
		cache:      make(map[string]*cacheEntry),
		rawIndex:   make(map[string]string),
		inflight:   make(map[string]*compileCall),
		lru:        list.New(),
	}
}

func badFilter(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrBadFilter}, args...)...)
}

func badSnapshot(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrBadSnapshot}, args...)...)
}

func attrRefString(ref AttrRef) string {
	if ref.Sub == "" {
		return ref.Path
	}
	return ref.Path + "." + ref.Sub
}

func makeAttrRef(path string) AttrRef {
	if index := strings.LastIndex(path, "."); index >= 0 && index < len(path)-1 {
		return AttrRef{Path: path[:index], Sub: path[index+1:]}
	}
	return AttrRef{Path: path}
}

func canonicalSchema(schema AttrSchema) AttrSchema {
	if schema.Sub == "" {
		ref := makeAttrRef(schema.Path)
		schema.Path = ref.Path
		schema.Sub = ref.Sub
	}
	if schema.Kind == "" {
		schema.Kind = KindString
	}
	return schema
}

func attributeValues(attrs map[string][]Value, ref AttrRef) []Value {
	if values, ok := attrs[attrRefString(ref)]; ok && len(values) > 0 {
		return values
	}
	if ref.Sub != "" {
		if values, ok := attrs[ref.Path]; ok && len(values) > 0 {
			return values
		}
	}
	return nil
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func tokenizeFilter(filter string) ([]token, error) {
	tokens := make([]token, 0, 8)
	i := 0
	for i < len(filter) {
		c := filter[i]
		if isSpaceByte(c) {
			i++
			continue
		}
		switch c {
		case '(':
			tokens = append(tokens, token{kind: tokenLParen, text: "("})
			i++
			continue
		case ')':
			tokens = append(tokens, token{kind: tokenRParen, text: ")"})
			i++
			continue
		case '"':
			start := i
			i++
			closed := false
			for i < len(filter) {
				if filter[i] == '\\' {
					i++
					if i >= len(filter) {
						return nil, badFilter("unterminated string escape")
					}
					i++
					continue
				}
				if filter[i] == '"' {
					literal := filter[start : i+1]
					decoded, err := strconv.Unquote(literal)
					if err != nil {
						return nil, badFilter("invalid string literal %q", literal)
					}
					tokens = append(tokens, token{kind: tokenString, text: decoded, value: Value{Kind: KindString, Str: decoded}})
					closed = true
					i++
					break
				}
				i++
			}
			if !closed {
				return nil, badFilter("unterminated string literal")
			}
			continue
		}

		if isDigitByte(c) || (c == '-' && i+1 < len(filter) && isDigitByte(filter[i+1])) {
			start := i
			i++
			for i < len(filter) {
				d := filter[i]
				if isDigitByte(d) || d == '.' || d == 'e' || d == 'E' {
					i++
					continue
				}
				if (d == '+' || d == '-') && i > start && (filter[i-1] == 'e' || filter[i-1] == 'E') {
					i++
					continue
				}
				break
			}
			text := filter[start:i]
			number, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, badFilter("invalid number %q", text)
			}
			tokens = append(tokens, token{kind: tokenNumber, text: text, value: Value{Kind: KindNumber, Num: number}})
			continue
		}

		start := i
		for i < len(filter) {
			d := filter[i]
			if isSpaceByte(d) || d == '(' || d == ')' || d == '"' {
				break
			}
			i++
		}
		text := filter[start:i]
		switch {
		case strings.EqualFold(text, "true"):
			tokens = append(tokens, token{kind: tokenBool, text: text, value: Value{Kind: KindBool, Bool: true}})
		case strings.EqualFold(text, "false"):
			tokens = append(tokens, token{kind: tokenBool, text: text, value: Value{Kind: KindBool, Bool: false}})
		default:
			tokens = append(tokens, token{kind: tokenIdent, text: text})
		}
	}
	tokens = append(tokens, token{kind: tokenEOF})
	return tokens, nil
}

func buildFilterAST(filter string) (exprNode, error) {
	tokens, err := tokenizeFilter(filter)
	if err != nil {
		return nil, err
	}
	parser := &filterParser{tokens: tokens}
	root, err := parser.parseOr()
	if err != nil {
		return nil, err
	}
	if parser.current().kind != tokenEOF {
		return nil, badFilter("unexpected token %q", parser.current().text)
	}
	return root, nil
}

func (p *filterParser) current() token {
	if p.pos >= len(p.tokens) {
		return token{kind: tokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *filterParser) advance() token {
	current := p.current()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return current
}

func isKeywordToken(tok token, keyword string) bool {
	return tok.kind == tokenIdent && strings.EqualFold(tok.text, keyword)
}

func (p *filterParser) parseOr() (exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for isKeywordToken(p.current(), boolOr) {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: boolOr, left: left, right: right}
	}
	return left, nil
}

func (p *filterParser) parseAnd() (exprNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for isKeywordToken(p.current(), boolAnd) {
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: boolAnd, left: left, right: right}
	}
	return left, nil
}

func (p *filterParser) parseUnary() (exprNode, error) {
	if isKeywordToken(p.current(), boolNot) {
		p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &notNode{operand: operand}, nil
	}
	if p.current().kind == tokenLParen {
		p.advance()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.current().kind != tokenRParen {
			return nil, badFilter("missing closing parenthesis")
		}
		p.advance()
		return inner, nil
	}
	return p.parseComparison()
}

func normalizeOperator(op string) (string, error) {
	switch strings.ToLower(op) {
	case OpEq, "=", "==":
		return OpEq, nil
	case OpNe, "!=", "<>":
		return OpNe, nil
	case OpCo:
		return OpCo, nil
	case OpSw:
		return OpSw, nil
	case OpEw:
		return OpEw, nil
	case OpPr:
		return OpPr, nil
	case OpGt, ">":
		return OpGt, nil
	case OpGe, ">=":
		return OpGe, nil
	case OpLt, "<":
		return OpLt, nil
	case OpLe, "<=":
		return OpLe, nil
	default:
		return "", badFilter("unknown operator %q", op)
	}
}

func (p *filterParser) parseComparison() (exprNode, error) {
	attrToken := p.advance()
	if attrToken.kind != tokenIdent || strings.TrimSpace(attrToken.text) == "" {
		return nil, badFilter("expected an attribute path")
	}
	opToken := p.advance()
	if opToken.kind != tokenIdent {
		return nil, badFilter("attribute %q is missing an operator", attrToken.text)
	}
	op, err := normalizeOperator(opToken.text)
	if err != nil {
		return nil, err
	}
	node := &compareNode{ref: makeAttrRef(attrToken.text), op: op}
	if op == OpPr {
		return node, nil
	}
	valueToken := p.advance()
	switch valueToken.kind {
	case tokenString, tokenNumber, tokenBool:
		node.literal = valueToken.value
		node.hasLiteral = true
	default:
		return nil, badFilter("operator %q requires a right-hand value", op)
	}
	return node, nil
}

func prepareExpression(node exprNode, schemas map[AttrRef]AttrSchema) error {
	switch n := node.(type) {
	case *binaryNode:
		if err := prepareExpression(n.left, schemas); err != nil {
			return err
		}
		return prepareExpression(n.right, schemas)
	case *notNode:
		return prepareExpression(n.operand, schemas)
	case *compareNode:
		schema, ok := schemas[n.ref]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownAttr, attrRefString(n.ref))
		}
		n.schema = schema
		return validateComparison(n)
	default:
		return badFilter("empty expression")
	}
}

func validateComparison(n *compareNode) error {
	if n.op == OpPr {
		return nil
	}
	if !n.hasLiteral {
		return badFilter("operator %q requires a right-hand value", n.op)
	}
	switch n.schema.Kind {
	case KindString:
		switch n.op {
		case OpEq, OpNe, OpCo, OpSw, OpEw, OpGt, OpGe, OpLt, OpLe:
		default:
			return badFilter("operator %q is invalid for a string attribute", n.op)
		}
		if n.literal.Kind != KindString {
			return badFilter("operator %q requires a string value", n.op)
		}
	case KindNumber:
		switch n.op {
		case OpEq, OpNe, OpGt, OpGe, OpLt, OpLe:
		default:
			return badFilter("operator %q is invalid for a numeric attribute", n.op)
		}
		if n.literal.Kind != KindNumber {
			return badFilter("operator %q requires a number value", n.op)
		}
	case KindBool:
		if n.op != OpEq && n.op != OpNe {
			return badFilter("boolean attributes support only eq and ne")
		}
		if n.literal.Kind != KindBool {
			return badFilter("operator %q requires a boolean value", n.op)
		}
	default:
		return badFilter("unsupported schema kind %q", n.schema.Kind)
	}
	return nil
}

func writeCanonicalValue(builder *strings.Builder, value Value) {
	switch value.Kind {
	case KindString:
		builder.WriteString("s:")
		builder.WriteString(strconv.Quote(value.Str))
	case KindNumber:
		number := value.Num
		if number == 0 {
			number = 0
		}
		builder.WriteString("n:")
		builder.WriteString(strconv.FormatFloat(number, 'g', -1, 64))
	case KindBool:
		builder.WriteString("b:")
		builder.WriteString(strconv.FormatBool(value.Bool))
	default:
		builder.WriteString("invalid")
	}
}

func writeCanonicalNode(builder *strings.Builder, node exprNode) {
	switch n := node.(type) {
	case *binaryNode:
		builder.WriteByte('(')
		writeCanonicalNode(builder, n.left)
		builder.WriteByte(' ')
		builder.WriteString(n.op)
		builder.WriteByte(' ')
		writeCanonicalNode(builder, n.right)
		builder.WriteByte(')')
	case *notNode:
		builder.WriteString("(not ")
		writeCanonicalNode(builder, n.operand)
		builder.WriteByte(')')
	case *compareNode:
		builder.WriteString(attrRefString(n.ref))
		builder.WriteByte(' ')
		builder.WriteString(n.op)
		if n.op != OpPr {
			builder.WriteByte(' ')
			writeCanonicalValue(builder, n.literal)
		}
	}
}

func fingerprintNode(node exprNode) string {
	var builder strings.Builder
	builder.WriteString("scim-filter-v1\n")
	writeCanonicalNode(&builder, node)
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func collectRefs(node exprNode, refs *[]AttrRef) {
	switch n := node.(type) {
	case *binaryNode:
		collectRefs(n.left, refs)
		collectRefs(n.right, refs)
	case *notNode:
		collectRefs(n.operand, refs)
	case *compareNode:
		*refs = append(*refs, n.ref)
	}
}

func normalizeStringCompare(value string, caseExact bool) string {
	if caseExact {
		return value
	}
	return strings.ToLower(value)
}

func compareStrings(left, right string, caseExact bool) int {
	return strings.Compare(normalizeStringCompare(left, caseExact), normalizeStringCompare(right, caseExact))
}

func compareCondition(schema AttrSchema, op string, literal, actual Value) (bool, error) {
	switch schema.Kind {
	case KindString:
		cmp := compareStrings(actual.Str, literal.Str, schema.CaseExact)
		switch op {
		case OpEq:
			return cmp == 0, nil
		case OpNe:
			return cmp != 0, nil
		case OpGt:
			return cmp > 0, nil
		case OpGe:
			return cmp >= 0, nil
		case OpLt:
			return cmp < 0, nil
		case OpLe:
			return cmp <= 0, nil
		case OpCo:
			normalizedActual := normalizeStringCompare(actual.Str, schema.CaseExact)
			normalizedLiteral := normalizeStringCompare(literal.Str, schema.CaseExact)
			return strings.Contains(normalizedActual, normalizedLiteral), nil
		case OpSw:
			normalizedActual := normalizeStringCompare(actual.Str, schema.CaseExact)
			normalizedLiteral := normalizeStringCompare(literal.Str, schema.CaseExact)
			return strings.HasPrefix(normalizedActual, normalizedLiteral), nil
		case OpEw:
			normalizedActual := normalizeStringCompare(actual.Str, schema.CaseExact)
			normalizedLiteral := normalizeStringCompare(literal.Str, schema.CaseExact)
			return strings.HasSuffix(normalizedActual, normalizedLiteral), nil
		default:
			return false, fmt.Errorf("unsupported string operator %q", op)
		}
	case KindNumber:
		switch op {
		case OpEq:
			return actual.Num == literal.Num, nil
		case OpNe:
			return actual.Num != literal.Num, nil
		case OpGt:
			return actual.Num > literal.Num, nil
		case OpGe:
			return actual.Num >= literal.Num, nil
		case OpLt:
			return actual.Num < literal.Num, nil
		case OpLe:
			return actual.Num <= literal.Num, nil
		default:
			return false, fmt.Errorf("unsupported numeric operator %q", op)
		}
	case KindBool:
		switch op {
		case OpEq:
			return actual.Bool == literal.Bool, nil
		case OpNe:
			return actual.Bool != literal.Bool, nil
		default:
			return false, fmt.Errorf("unsupported boolean operator %q", op)
		}
	default:
		return false, fmt.Errorf("unsupported value kind %q", schema.Kind)
	}
}

func evalNode(node exprNode, resource Resource) (bool, error) {
	switch n := node.(type) {
	case *binaryNode:
		left, err := evalNode(n.left, resource)
		if err != nil {
			return false, err
		}
		if n.op == boolAnd {
			if !left {
				return false, nil
			}
			return evalNode(n.right, resource)
		}
		if left {
			return true, nil
		}
		return evalNode(n.right, resource)
	case *notNode:
		value, err := evalNode(n.operand, resource)
		if err != nil {
			return false, err
		}
		return !value, nil
	case *compareNode:
		return evalComparison(n, resource)
	default:
		return false, ErrBadFilter
	}
}

func evalComparison(n *compareNode, resource Resource) (bool, error) {
	values := attributeValues(resource.Attrs, n.ref)
	if len(values) == 0 {
		return n.op == OpNe, nil
	}
	if n.op == OpPr {
		return true, nil
	}
	for _, value := range values {
		matched, err := compareCondition(n.schema, n.op, n.literal, value)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func cloneResource(resource Resource) Resource {
	clone := resource
	if resource.Attrs != nil {
		clone.Attrs = make(map[string][]Value, len(resource.Attrs))
		for key, values := range resource.Attrs {
			clone.Attrs[key] = append([]Value(nil), values...)
		}
	}
	return clone
}

// RegisterSchema adds one attribute path to the directory schema.
func (e *ScimFilterEngine) RegisterSchema(schema AttrSchema) error {
	schema = canonicalSchema(schema)
	if schema.Path == "" {
		return fmt.Errorf("%w: attribute path is empty", ErrBadFilter)
	}
	switch schema.Kind {
	case KindString, KindNumber, KindBool:
	default:
		return fmt.Errorf("%w: unsupported kind %q", ErrBadFilter, schema.Kind)
	}
	ref := AttrRef{Path: schema.Path, Sub: schema.Sub}

	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.schemas == nil {
		e.schemas = make(map[AttrRef]AttrSchema)
	}
	if _, exists := e.schemas[ref]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateSchema, attrRefString(ref))
	}
	e.schemas[ref] = schema
	return nil
}

func (e *ScimFilterEngine) parse(filter string) (Expr, error) {
	if strings.TrimSpace(filter) == "" {
		return Expr{}, badFilter("filter is empty")
	}
	root, err := buildFilterAST(filter)
	if err != nil {
		return Expr{}, err
	}

	e.stateMu.RLock()
	schemas := e.schemas
	err = prepareExpression(root, schemas)
	e.stateMu.RUnlock()
	if err != nil {
		return Expr{}, err
	}

	return Expr{text: filter, fingerprint: fingerprintNode(root), root: root}, nil
}

// Parse compiles one filter string into a reusable expression.
func (e *ScimFilterEngine) Parse(filter string) (Expr, error) {
	return e.parse(filter)
}

// Match evaluates an expression against one resource.
func (e *ScimFilterEngine) Match(expr Expr, res Resource) (bool, error) {
	if expr.root == nil {
		return false, fmt.Errorf("%w: expression has not been parsed", ErrBadFilter)
	}
	return evalNode(expr.root, res)
}

// Explain lists the attribute references an expression touches.
func (e *ScimFilterEngine) Explain(expr Expr) []AttrRef {
	if expr.root == nil {
		return nil
	}
	refs := make([]AttrRef, 0)
	collectRefs(expr.root, &refs)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Path != refs[j].Path {
			return refs[i].Path < refs[j].Path
		}
		return refs[i].Sub < refs[j].Sub
	})
	deduped := make([]AttrRef, 0, len(refs))
	for _, ref := range refs {
		if len(deduped) > 0 && deduped[len(deduped)-1] == ref {
			continue
		}
		deduped = append(deduped, ref)
	}
	return deduped
}

// Fingerprint returns the stable identity of a compiled expression.
func (e *ScimFilterEngine) Fingerprint(expr Expr) string {
	if expr.fingerprint != "" {
		return expr.fingerprint
	}
	if expr.root != nil {
		return fingerprintNode(expr.root)
	}
	return ""
}

// Put stores or replaces one resource.
func (e *ScimFilterEngine) Put(res Resource) error {
	if res.Kind == "" {
		return fmt.Errorf("%w: empty resource kind", ErrUnknownKind)
	}
	if res.ID == "" {
		return errors.New("resource id is required")
	}

	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.resources == nil {
		e.resources = make(map[string]map[string]Resource)
	}
	if e.tombstones == nil {
		e.tombstones = make(map[string]map[string]uint64)
	}
	if _, ok := e.resources[res.Kind]; !ok {
		e.resources[res.Kind] = make(map[string]Resource)
	}
	e.resources[res.Kind][res.ID] = cloneResource(res)
	if kindTombstones, ok := e.tombstones[res.Kind]; ok {
		delete(kindTombstones, res.ID)
	}
	e.sequence++
	return nil
}

// Get reads one resource by kind and id.
func (e *ScimFilterEngine) Get(kind, id string) (Resource, bool) {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	kindResources, ok := e.resources[kind]
	if !ok {
		return Resource{}, false
	}
	resource, ok := kindResources[id]
	if !ok {
		return Resource{}, false
	}
	return cloneResource(resource), true
}

// Delete removes one resource and leaves a tombstone behind.
func (e *ScimFilterEngine) Delete(kind, id string) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.resources == nil {
		e.resources = make(map[string]map[string]Resource)
	}
	if e.tombstones == nil {
		e.tombstones = make(map[string]map[string]uint64)
	}

	_, activeKind := e.resources[kind]
	_, tombstoneKind := e.tombstones[kind]
	if !activeKind && !tombstoneKind {
		return fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if _, ok := e.resources[kind][id]; !ok {
		return fmt.Errorf("%w: %s/%s", ErrNotFound, kind, id)
	}

	delete(e.resources[kind], id)
	if _, ok := e.tombstones[kind]; !ok {
		e.tombstones[kind] = make(map[string]uint64)
	}
	e.sequence++
	e.tombstones[kind][id] = e.sequence
	return nil
}

// CacheStats reports the compiled expression cache.
func (e *ScimFilterEngine) CacheStats() CacheStats {
	e.cacheMu.RLock()
	defer e.cacheMu.RUnlock()
	return CacheStats{Entries: len(e.cache), Hits: e.cacheHits, Misses: e.cacheMiss}
}

func (e *ScimFilterEngine) compiledExpression(filter string) (Expr, error) {
	e.cacheMu.Lock()
	if e.cache == nil {
		e.cache = make(map[string]*cacheEntry)
		e.rawIndex = make(map[string]string)
		e.inflight = make(map[string]*compileCall)
		e.lru = list.New()
	}
	if fingerprint, ok := e.rawIndex[filter]; ok {
		if entry, ok := e.cache[fingerprint]; ok {
			e.cacheHits++
			e.lru.MoveToFront(entry.elem)
			expr := entry.expr
			e.cacheMu.Unlock()
			return expr, nil
		}
		delete(e.rawIndex, filter)
	}
	if call, ok := e.inflight[filter]; ok {
		e.cacheMu.Unlock()
		<-call.done
		if call.err != nil {
			return Expr{}, call.err
		}
		e.cacheMu.Lock()
		e.cacheHits++
		e.cacheMu.Unlock()
		return call.expr, nil
	}

	call := &compileCall{done: make(chan struct{})}
	e.inflight[filter] = call
	e.cacheMiss++
	e.cacheMu.Unlock()

	expr, err := e.Parse(filter)

	e.cacheMu.Lock()
	delete(e.inflight, filter)
	if err != nil {
		call.err = err
		close(call.done)
		e.cacheMu.Unlock()
		return Expr{}, err
	}

	call.expr = expr
	if e.limits.MaxCacheEntries > 0 {
		entry, exists := e.cache[expr.fingerprint]
		if !exists {
			entry = &cacheEntry{
				expr: expr,
				raws: make(map[string]struct{}),
				elem: e.lru.PushFront(expr.fingerprint),
			}
			e.cache[expr.fingerprint] = entry
		} else {
			entry.expr = expr
			e.lru.MoveToFront(entry.elem)
		}
		entry.raws[filter] = struct{}{}
		e.rawIndex[filter] = expr.fingerprint
		e.evictCacheLocked()
	}
	close(call.done)
	e.cacheMu.Unlock()
	return expr, nil
}

func (e *ScimFilterEngine) evictCacheLocked() {
	for e.limits.MaxCacheEntries > 0 && e.lru.Len() > e.limits.MaxCacheEntries {
		oldest := e.lru.Back()
		if oldest == nil {
			return
		}
		fingerprint, _ := oldest.Value.(string)
		if entry, ok := e.cache[fingerprint]; ok {
			for raw := range entry.raws {
				delete(e.rawIndex, raw)
			}
			delete(e.cache, fingerprint)
		}
		e.lru.Remove(oldest)
	}
}

func (e *ScimFilterEngine) matchingResources(kind string, expr Expr) ([]Resource, error) {
	e.stateMu.RLock()
	kindResources, activeKind := e.resources[kind]
	_, tombstoneKind := e.tombstones[kind]
	if !activeKind && !tombstoneKind {
		e.stateMu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	rows := make([]Resource, 0, len(kindResources))
	for _, resource := range kindResources {
		rows = append(rows, cloneResource(resource))
	}
	e.stateMu.RUnlock()

	matched := make([]Resource, 0, len(rows))
	for _, resource := range rows {
		ok, err := evalNode(expr.root, resource)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, resource)
		}
	}
	return matched, nil
}

func (e *ScimFilterEngine) newSortPlan(sortBy string, rows []Resource) sortPlan {
	sortBy = strings.TrimSpace(sortBy)
	if sortBy == "" || strings.EqualFold(sortBy, "id") {
		return sortPlan{}
	}
	ref := makeAttrRef(sortBy)
	e.stateMu.RLock()
	schema, registered := e.schemas[ref]
	e.stateMu.RUnlock()
	if !registered {
		kind := ValueKind("")
		for _, resource := range rows {
			if values := attributeValues(resource.Attrs, ref); len(values) > 0 {
				kind = values[0].Kind
				break
			}
		}
		if kind == "" {
			kind = KindString
		}
		schema = AttrSchema{Path: ref.Path, Sub: ref.Sub, Kind: kind, CaseExact: true}
	}
	return sortPlan{active: true, ref: ref, schema: schema}
}

func (p sortPlan) key() string {
	if !p.active {
		return ""
	}
	return attrRefString(p.ref)
}

func (p sortPlan) tuple(resource Resource) sortTuple {
	if !p.active {
		return sortTuple{id: resource.ID}
	}
	values := attributeValues(resource.Attrs, p.ref)
	if len(values) == 0 {
		return sortTuple{id: resource.ID}
	}
	return sortTuple{present: true, value: values[0], id: resource.ID}
}

func compareSortValues(left, right Value, schema AttrSchema) int {
	kind := schema.Kind
	if kind == "" {
		kind = left.Kind
	}
	if kind == "" {
		kind = right.Kind
	}
	switch kind {
	case KindNumber:
		if left.Num < right.Num {
			return -1
		}
		if left.Num > right.Num {
			return 1
		}
		return 0
	case KindBool:
		if left.Bool == right.Bool {
			return 0
		}
		if !left.Bool {
			return -1
		}
		return 1
	default:
		return compareStrings(left.Str, right.Str, schema.CaseExact)
	}
}

func (p sortPlan) compareTuple(left, right sortTuple) int {
	if p.active {
		if left.present != right.present {
			if !left.present {
				return -1
			}
			return 1
		}
		if left.present {
			if cmp := compareSortValues(left.value, right.value, p.schema); cmp != 0 {
				return cmp
			}
		}
	}
	return strings.Compare(left.id, right.id)
}

func encodeCursor(cursor storedCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCursorInvalid, err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, cursorHMACKey)
	_, _ = mac.Write(raw)
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return cursorPrefix + "." + payload + "." + signature, nil
}

func decodeCursor(token string) (*storedCursor, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != cursorPrefix {
		return nil, fmt.Errorf("%w: wrong format", ErrCursorInvalid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload encoding", ErrCursorInvalid)
	}
	providedSignature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding", ErrCursorInvalid)
	}
	mac := hmac.New(sha256.New, cursorHMACKey)
	_, _ = mac.Write(raw)
	if !hmac.Equal(providedSignature, mac.Sum(nil)) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrCursorInvalid)
	}

	var cursor storedCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return nil, fmt.Errorf("%w: payload json", ErrCursorInvalid)
	}
	if cursor.Version != cursorVersion || cursor.Fingerprint == "" || cursor.ID == "" {
		return nil, fmt.Errorf("%w: incomplete payload", ErrCursorInvalid)
	}
	if cursor.HasValue && cursor.Value == nil {
		return nil, fmt.Errorf("%w: missing sort value", ErrCursorInvalid)
	}
	return &cursor, nil
}

// Query filters, orders and pages the resources of one kind.
func (e *ScimFilterEngine) Query(kind string, req QueryRequest) (QueryPage, error) {
	expr, err := e.compiledExpression(req.Filter)
	if err != nil {
		return QueryPage{}, err
	}
	fingerprint := e.Fingerprint(expr)

	rows, err := e.matchingResources(kind, expr)
	if err != nil {
		return QueryPage{}, err
	}
	total := len(rows)
	if total > e.limits.MaxResults {
		return QueryPage{}, fmt.Errorf("%w: %d matches exceed %d", ErrResultLimit, total, e.limits.MaxResults)
	}

	plan := e.newSortPlan(req.SortBy, rows)
	plan.descending = req.Descending
	sort.Slice(rows, func(i, j int) bool {
		cmp := plan.compareTuple(plan.tuple(rows[i]), plan.tuple(rows[j]))
		if plan.descending {
			return cmp > 0
		}
		return cmp < 0
	})

	var cursor *storedCursor
	if strings.TrimSpace(req.Cursor) != "" {
		cursor, err = decodeCursor(req.Cursor)
		if err != nil {
			return QueryPage{}, err
		}
		if cursor.Fingerprint != fingerprint {
			return QueryPage{}, fmt.Errorf("%w: cursor fingerprint does not match query", ErrCursorMismatch)
		}
		if cursor.Kind != kind || cursor.Sort != plan.key() || cursor.Descending != req.Descending {
			return QueryPage{}, fmt.Errorf("%w: cursor query context does not match", ErrCursorMismatch)
		}
	}

	start := 0
	if cursor != nil {
		cursorTuple := sortTuple{id: cursor.ID, present: cursor.HasValue}
		if cursor.Value != nil {
			cursorTuple.value = *cursor.Value
		}
		for start < len(rows) {
			cmp := plan.compareTuple(cursorTuple, plan.tuple(rows[start]))
			cursorBefore := cmp < 0
			if plan.descending {
				cursorBefore = cmp > 0
			}
			if cursorBefore {
				break
			}
			start++
		}
	}

	after := rows[start:]
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = e.limits.MaxPageSize
	}
	if pageSize > e.limits.MaxPageSize {
		pageSize = e.limits.MaxPageSize
	}

	page := []Resource{}
	if pageSize > 0 {
		end := len(after)
		if end > pageSize {
			end = pageSize
		}
		page = append(page, after[:end]...)
	}

	var nextCursor string
	if pageSize > 0 && len(after) > pageSize {
		last := page[len(page)-1]
		tuple := plan.tuple(last)
		payload := storedCursor{
			Version:     cursorVersion,
			Fingerprint: fingerprint,
			Kind:        kind,
			Sort:        plan.key(),
			Descending:  req.Descending,
			ID:          last.ID,
			HasValue:    tuple.present,
		}
		if tuple.present {
			value := tuple.value
			payload.Value = &value
		}
		nextCursor, err = encodeCursor(payload)
		if err != nil {
			return QueryPage{}, err
		}
	}

	return QueryPage{Resources: page, NextCursor: nextCursor, Total: total, Fingerprint: fingerprint}, nil
}

// Snapshot serialises schema, resources, tombstones, sequence and cache data.
func (e *ScimFilterEngine) Snapshot() ([]byte, error) {
	dto := snapshotData{Magic: snapshotMagic, Version: snapshotVersion}

	e.stateMu.RLock()
	dto.Sequence = e.sequence
	dto.Schemas = make([]AttrSchema, 0, len(e.schemas))
	for _, schema := range e.schemas {
		dto.Schemas = append(dto.Schemas, schema)
	}
	dto.Resources = make([]Resource, 0)
	for _, kindResources := range e.resources {
		for _, resource := range kindResources {
			dto.Resources = append(dto.Resources, cloneResource(resource))
		}
	}
	dto.Tombstones = make([]tombstoneRecord, 0)
	for kind, ids := range e.tombstones {
		for id, sequence := range ids {
			dto.Tombstones = append(dto.Tombstones, tombstoneRecord{Kind: kind, ID: id, Sequence: sequence})
		}
	}
	e.stateMu.RUnlock()

	sort.Slice(dto.Schemas, func(i, j int) bool {
		if dto.Schemas[i].Path != dto.Schemas[j].Path {
			return dto.Schemas[i].Path < dto.Schemas[j].Path
		}
		return dto.Schemas[i].Sub < dto.Schemas[j].Sub
	})
	sort.Slice(dto.Resources, func(i, j int) bool {
		if dto.Resources[i].Kind != dto.Resources[j].Kind {
			return dto.Resources[i].Kind < dto.Resources[j].Kind
		}
		return dto.Resources[i].ID < dto.Resources[j].ID
	})
	sort.Slice(dto.Tombstones, func(i, j int) bool {
		if dto.Tombstones[i].Kind != dto.Tombstones[j].Kind {
			return dto.Tombstones[i].Kind < dto.Tombstones[j].Kind
		}
		return dto.Tombstones[i].ID < dto.Tombstones[j].ID
	})

	e.cacheMu.RLock()
	dto.Cache = make([]cacheSnapshot, 0, len(e.rawIndex))
	for raw, fingerprint := range e.rawIndex {
		dto.Cache = append(dto.Cache, cacheSnapshot{Filter: raw, Fingerprint: fingerprint})
	}
	dto.CacheHits = e.cacheHits
	dto.CacheMisses = e.cacheMiss
	e.cacheMu.RUnlock()
	sort.Slice(dto.Cache, func(i, j int) bool { return dto.Cache[i].Filter < dto.Cache[j].Filter })

	data, err := json.Marshal(dto)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Restore replaces all engine state from a validated snapshot.
func (e *ScimFilterEngine) Restore(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return badSnapshot("empty payload")
	}
	var dto snapshotData
	if err := json.Unmarshal(data, &dto); err != nil {
		return badSnapshot("json decode: %v", err)
	}
	if dto.Magic != snapshotMagic || dto.Version != snapshotVersion {
		return badSnapshot("unsupported snapshot version")
	}
	if dto.CacheHits < 0 || dto.CacheMisses < 0 {
		return badSnapshot("negative cache statistics")
	}

	schemas := make(map[AttrRef]AttrSchema, len(dto.Schemas))
	for _, schema := range dto.Schemas {
		schema = canonicalSchema(schema)
		ref := AttrRef{Path: schema.Path, Sub: schema.Sub}
		if ref.Path == "" {
			return badSnapshot("schema with empty path")
		}
		switch schema.Kind {
		case KindString, KindNumber, KindBool:
		default:
			return badSnapshot("schema %s has invalid kind %q", attrRefString(ref), schema.Kind)
		}
		if _, exists := schemas[ref]; exists {
			return badSnapshot("duplicate schema %s", attrRefString(ref))
		}
		schemas[ref] = schema
	}

	resources := make(map[string]map[string]Resource)
	resourceKeys := make(map[string]struct{})
	for _, resource := range dto.Resources {
		if resource.Kind == "" || resource.ID == "" {
			return badSnapshot("resource missing kind or id")
		}
		key := resource.Kind + "\x00" + resource.ID
		if _, exists := resourceKeys[key]; exists {
			return badSnapshot("duplicate resource %s/%s", resource.Kind, resource.ID)
		}
		resourceKeys[key] = struct{}{}
		if resource.Attrs == nil {
			resource.Attrs = make(map[string][]Value)
		}
		for _, values := range resource.Attrs {
			for _, value := range values {
				switch value.Kind {
				case KindString, KindNumber, KindBool, "":
				default:
					return badSnapshot("resource %s/%s has invalid value kind", resource.Kind, resource.ID)
				}
			}
		}
		if _, ok := resources[resource.Kind]; !ok {
			resources[resource.Kind] = make(map[string]Resource)
		}
		resources[resource.Kind][resource.ID] = resource
	}

	tombstones := make(map[string]map[string]uint64)
	tombstoneKeys := make(map[string]struct{})
	for _, tombstone := range dto.Tombstones {
		if tombstone.Kind == "" || tombstone.ID == "" {
			return badSnapshot("tombstone missing kind or id")
		}
		key := tombstone.Kind + "\x00" + tombstone.ID
		if _, exists := tombstoneKeys[key]; exists {
			return badSnapshot("duplicate tombstone %s/%s", tombstone.Kind, tombstone.ID)
		}
		tombstoneKeys[key] = struct{}{}
		if tombstone.Sequence > dto.Sequence {
			return badSnapshot("tombstone sequence is newer than snapshot sequence")
		}
		if _, active := resources[tombstone.Kind][tombstone.ID]; active {
			return badSnapshot("tombstoned resource %s/%s is active", tombstone.Kind, tombstone.ID)
		}
		if _, ok := tombstones[tombstone.Kind]; !ok {
			tombstones[tombstone.Kind] = make(map[string]uint64)
		}
		tombstones[tombstone.Kind][tombstone.ID] = tombstone.Sequence
	}
	for _, kind := range []string{"User", "Group"} {
		if resources[kind] == nil {
			resources[kind] = make(map[string]Resource)
		}
		if tombstones[kind] == nil {
			tombstones[kind] = make(map[string]uint64)
		}
	}

	restoredCache := make(map[string]*cacheEntry)
	restoredRawIndex := make(map[string]string)
	restoredLRU := list.New()
	tempEngine := &ScimFilterEngine{schemas: schemas}
	for _, item := range dto.Cache {
		expr, err := tempEngine.Parse(item.Filter)
		if err != nil {
			return badSnapshot("cached filter cannot be compiled: %v", err)
		}
		if item.Fingerprint != "" && item.Fingerprint != expr.fingerprint {
			return badSnapshot("cached fingerprint mismatch")
		}
		if e.limits.MaxCacheEntries <= 0 {
			continue
		}
		fingerprint := expr.fingerprint
		entry, exists := restoredCache[fingerprint]
		if !exists {
			entry = &cacheEntry{
				expr: expr,
				raws: make(map[string]struct{}),
				elem: restoredLRU.PushFront(fingerprint),
			}
			restoredCache[fingerprint] = entry
		}
		entry.raws[item.Filter] = struct{}{}
		restoredRawIndex[item.Filter] = fingerprint
		for restoredLRU.Len() > e.limits.MaxCacheEntries {
			oldest := restoredLRU.Back()
			if oldest == nil {
				break
			}
			oldestFingerprint, _ := oldest.Value.(string)
			if oldEntry, ok := restoredCache[oldestFingerprint]; ok {
				for raw := range oldEntry.raws {
					delete(restoredRawIndex, raw)
				}
				delete(restoredCache, oldestFingerprint)
			}
			restoredLRU.Remove(oldest)
		}
	}

	e.stateMu.Lock()
	e.cacheMu.Lock()
	e.schemas = schemas
	e.resources = resources
	e.tombstones = tombstones
	e.sequence = dto.Sequence
	e.cache = restoredCache
	e.rawIndex = restoredRawIndex
	e.inflight = make(map[string]*compileCall)
	e.lru = restoredLRU
	e.cacheHits = dto.CacheHits
	e.cacheMiss = dto.CacheMisses
	e.cacheMu.Unlock()
	e.stateMu.Unlock()
	return nil
}
