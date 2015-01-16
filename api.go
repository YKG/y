// Copyright 2014 The y Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//TODO +Engine, and export it.

// Package y converts .y (yacc[2]) source files to data suitable for a parser
// generator.
//
// Changelog
//
// 2015-01-16: Added Parser.Reductions.
//
// 2014-12-18: Support %precedence for better bison compatibility[5].
//
// Links
//
// Referenced from elsewhere:
//
//  [0]: http://godoc.org/github.com/cznic/goyacc
//  [1]: http://people.via.ecp.fr/~stilgar/doc/compilo/parser/Generating%20LR%20Syntax%20Error%20Messages.pdf
//  [2]: http://dinosaur.compilertools.net/yacc/
//  [3]: http://dinosaur.compilertools.net/lex/index.html
//  [4]: https://www.gnu.org/software/bison/manual/html_node/Using-Mid_002dRule-Actions.html
//  [5]: http://www.gnu.org/software/bison/manual/html_node/Precedence-Only.html#Precedence-Only
package y

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"io/ioutil"
	"sort"

	yparser "github.com/cznic/parser/yacc"
	yscanner "github.com/cznic/scanner/yacc"
)

// Values of {AssocDef,Rule,Sym}.Associativity
const (
	AssocNotSpecified = iota
	AssocLeft         // %left
	AssocRight        // %right
	AssocNone         // %nonassoc
	AssocPrecedence   // %precedence
)

// Action describes one cell of the parser table, ie. the action to be taken when
// the lookahead is Sym.
type Action struct {
	Sym *Symbol
	arg int
}

// Kind returns typ: 'a' for accept, 's' for shift, 'r' for reduce and 'g' for goto.
//
//	For 'a' arg is not used.
//	For 's' arg is the state number to shift to.
//	For 'r' arg is the rule number which is to be reduced.
//	For 'g' arg is the state number to goto.
func (a Action) Kind() (typ, arg int) {
	if !a.Sym.IsTerminal {
		return 'g', a.arg
	}

	switch arg := a.arg; {
	case arg < 0:
		return 'r', -arg
	case arg > 0:
		return 's', arg
	}

	return 'a', -1
}

// AssocDef describes one association definition of the .y source code. For
// example:
//
//	%left '+', '-'
//	%left '*', '/'
//
// The above will produce two items in Parser.AssocDefs with the particular
// values of the associativity and precendce recorded in the Associativity and
// Precedence fields of the respective Syms element.
type AssocDef struct {
	Associativity int       // One of the nonzero Assoc* constant values.
	Syms          []*Symbol // Symbols present for this association definition in the order of appearance.
}

// Options amend the behavior of the various Process* functions.
//
// Error Examples
//
// Error examples implement the ideas in "Generating LR Syntax Error Messages
// from Examples"[1]. They extend the capability of a LALR parser to produce
// better error messages.
//
// XErrorSrc is a sequence of Go tokens separated by white space using the same
// rules as valid Go source code except that semicolon injection is not used.
// Comments of both short and long form are equal to white space. An example
// consists of zero or more token specifiers followed by an error message.
// Token specifier is a valid Go identifier or a Go character literal. The
// error message is a Go string literal. The identifiers used in XErrorsSrc
// must be those defined as tokens in the yacc file. An implicit $end token is
// inserted at the end of the example input. For example:
//
//	/*
//		Reject empty file
//	*/
//	"invalid empty source file"
//
//	PACKAGE IDENT ';'
//	IMPORT STRING_LIT ','
//	"multiple imports must be separated by semicolons"
//
//	// Make the semicolon injection error a bit more user friendly.
//	PACKAGE ';'
//	`Missing package name or newline after "package"`
//
//	// A calculator parser might have error examples like
//	NUMBER '+' "operand expected"
//	NUMBER '-' error "invalid operand for subtraction"
//
// Use a specific bad token to provide a specific message:
//
//	// Coders frequently make this mistake.
//	FOO BAR BAZ "baz cannot follow bar, only qux or frob can"
//
// Use the reserved error token to be less specific:
//
//	// Catch any invalid token sequences after foo bar.
//	FOO BAR error "bar must be followed by qux or frob"
//
// Terminate the token sequence to detect premature end of file:
//
//	PACKAGE "missing package name"
//
// Similar to lex[3], examples sharing the same "action" can be joined by
// the | operator:
//
//	CONST  |
//	FUNC   |
//	IMPORT |
//	TYPE   |
//	VAR "package clause must be first"
//
// It's an error if the example token sequence is accepted by the parser, ie.
// if it does not produce an error.
type Options struct {
	AllowConflicts bool      // Do not report unresolved conflicts as errors.
	Closures       bool      // Report non kernel items.
	LA             bool      // Report all lookahead sets.
	Report         io.Writer // If non nil, write a grammar report to Report.
	Resolved       bool      // Explain how conflicts were resolved.
	XErrorsName    string    // Name used to report errors in XErrorsSrc, defaults to <xerrors>.
	XErrorsSrc     []byte    // Used to produce errors by example[1].

	debugSyms bool

	// In conflict with xerrors processing.
	noDefault bool // Disable collapsing largest reduce lookahead set to $default.
}

func (o *Options) boot(fset *token.FileSet) (*Options, error) {
	if o == nil {
		return &Options{}, nil
	}

	p := *o
	p.noDefault = true
	return &p, nil
}

// Parser describes the resulting parser. The intended client is a parser
// generator (like eg. [0]) producing the final Go source code.
type Parser struct {
	AssocDefs    []*AssocDef        // %left, %right, %nonassoc definitions in the order of appearance in the source code.
	ConflictsRR  int                // Number of reduce/reduce conflicts.
	ConflictsSR  int                // Number of shift/reduce conflicts.
	ErrorVerbose bool               // %error-verbose is present.
	Prologue     string             // Collected prologue between the %{ and %} marks.
	Rules        []*Rule            // Rules indexed by rule number.
	Start        string             // Name of the start production.
	States       []*State           // Parser states indexed by state number.
	Syms         map[string]*Symbol // Symbols indexed by name, eg. "IDENT", "Expression" or "';'".
	Table        [][]Action         // Indexed by state number.
	Tail         string             // Everyting after the second %%, if present.
	Union        *ast.StructType    // %union as Go AST.
	UnionSrc     string             // %union as Go source form.
	XErrors      []XError           // Errors by example[1] descriptions.
	y            *y
}

func newParser() *Parser {
	return &Parser{}
}

func (p *Parser) parse(stopState int, lex func() *Symbol) (int, error) {
	eof := p.Syms["$end"]
	yystate := 0
	var yyS []int
	var yychar *Symbol
stack:
	for i := 0; i < 100; i++ {
		if yystate == stopState {
			return yystate, nil
		}

		yyS = append(yyS, yystate)
		//dbg("yyS %v", yyS)
		if yychar == nil {
			yychar = lex()
			if yychar == nil {
				yychar = eof
			}
			//dbg("--------> LEX %s", yychar)
		}
		for _, act := range p.Table[yystate] {
			if act.Sym != yychar {
				continue
			}

			switch typ, arg := act.Kind(); typ {
			case 'a':
				//dbg("accept")
				return yystate, nil
			case 's':
				yychar = nil
				yystate = arg
				//dbg("shift and goto state %d", yystate)
			case 'r':
				rule := p.Rules[arg]
				n := len(yyS)
				m := len(rule.Components)
				yyS = yyS[:n-m]
				n -= m
				tos := yyS[n-1]
				yystate = p.States[tos].gotos[rule.Sym].arg
				//dbg("reduce rule %d and goto state %d", rule.RuleNum, yystate)
			}
			continue stack
		}
		return yystate, fmt.Errorf("no action for %s in state %d", yychar, yystate)
	}
	return yystate, fmt.Errorf("parser stall in state %d", yystate)
}

// Reductions returns a mapping rule# -> []state#. The slice is a sorted set of
// states in which the corresponding rule is reduced.
func (p *Parser) Reductions() map[int][]int {
	m := map[int][]int{}
	for state, actions := range p.Table {
		for _, act := range actions {
			if typ, arg := act.Kind(); typ == 'r' {
				m[arg] = append(m[arg], state)
			}
		}
	}
	for k, v := range m {
		sort.Ints(v)
		m[k] = v
	}
	return m
}

// ProcessAST processes yacc source code parsed in ast. It returns a *Parser or
// an error, if any.
func ProcessAST(fset *token.FileSet, ast *yparser.AST, opts *Options) (*Parser, error) {
	y, err := processAST(fset, ast, opts)
	if y == nil {
		return nil, err
	}

	if isTesting {
		y.Parser.y = y
	}

	for i, row := range y.Parser.Table {
		sort.Sort(actions(row))
		y.Parser.Table[i] = row
	}
	return y.Parser, err
}

// ProcessFile processes yacc source code in a named file. It returns a *Parser
// or an error, if any.
func ProcessFile(fset *token.FileSet, fname string, opts *Options) (*Parser, error) {
	b, err := ioutil.ReadFile(fname)
	if err != nil {
		return nil, err
	}

	return ProcessSource(fset, fname, b, opts)
}

// ProcessSource processes yacc source code in src. It returns a *Parser or an
// error, if any.
func ProcessSource(fset *token.FileSet, fname string, src []byte, opts *Options) (*Parser, error) {
	ast, err := yparser.Parse(fset, fname, src)
	if err != nil {
		return nil, err
	}

	return ProcessAST(fset, ast, opts)
}

// Rule describes a single yacc rule, for example (in source form)
//
//	Start:
//		Prologue Body Epilogue
//		{
//			$$ = &ast{$1, $2, $3}
//		}
//
// Inner rule actions
//
// A rule can prescribe semantic actions not only at the end. For example
//
//	Foo:
//		Bar
//		{
//			initBar($1)
//		}
//		Qux
//		{
//			handleQux($3)
//		}
//
// Such constructs are rewritten as
//
//	$@1:
//		{
//			initBar($1)
//		}
//
//	Foo:
//		Bar $@1 Qux
//		{
//			handleQux($3)
//		}
//
// The $@1 and similar is a synthetic rule and such have non nil Parent.
// MaxParentDlr is used to check that the semantic action does not access
// parent values not yet shifted to the parse stack as well as to compute the
// position of the $n thing on the parse stack. See also [4].
type Rule struct {
	Action          []*yparser.Act // Parts of the semantic action associated with the rule, if any.
	Associativity   int            // One of the assoc* constants.
	Components      []string       // Textual forms of the rule components, for example []string{"IDENT", "';'"}
	ExplicitPrecSym *Symbol        // Symbol used in the optional %prec sym clause, if present.
	MaxParentDlr    int            // See the Rule type docs for details.
	Parent          *Rule          // Non nil if a synthetic rule.
	PrecSym         *Symbol        // Effective %prec symbol used, if any.
	Precedence      int            // -1 if no precedence assigned.
	RuleNum         int            // Zero based rule number. Rule #0 is synthetic.
	Sym             *Symbol        // LHS of the rule.
	maxDlr          int
	pos             token.Pos
	syms            []*Symbol
}

// Actions returns the textual representation of r.Actions combined.
func (r *Rule) Actions() string {
	var buf bytes.Buffer
	for _, v := range r.Action {
		buf.WriteString(v.Src)
		switch v.Tok {
		case yscanner.DLR_DLR:
			buf.WriteString("$$")
		case yscanner.DLR_NUM:
			buf.WriteString(fmt.Sprintf("$%d", v.Num))
		case yscanner.DLR_TAG_DLR:
			buf.WriteString(fmt.Sprintf("$<%s>", v.Tag))
		case yscanner.DLR_TAG_NUM:
			buf.WriteString(fmt.Sprintf("$<%s>%d", v.Tag, v.Num))
		}
	}
	return buf.String()
}

// State represents one state of the parser.
type State struct {
	actions   map[*Symbol][]action //
	distance  int                  // On path to state 0.
	gotos     map[*Symbol]action   //
	id        int                  // Numeric id of the state.
	kernel    itemSet              //
	lookahead []symSet             // kernel LA.
	parent    *State               // On path to state 0.
	psym      *Symbol              // Label for the edge parent -> state.
	resolved  []string             //TODO non string data.
	sym       *Symbol              // Sym transfering from parent to state.
	trans     map[trans]stateItem  // sym.i -> stateItem
	xitems    itemSet              // {x ∈ closure(kernel) | x.rule -> ε }.
	xla       []symSet             // xitems LA.
	y         *y                   //
}

func newState(y *y, s itemSet) *State {
	return &State{
		actions:   map[*Symbol][]action{},
		gotos:     map[*Symbol]action{},
		kernel:    s,
		lookahead: make([]symSet, len(s)),
		trans:     map[trans]stateItem{},
		y:         y,
	}
}

func (s *State) zpath() []int {
	if s == nil {
		return nil
	}

	return append(s.parent.zpath(), s.id)
}

func (s *State) syms0() []*Symbol {
	s.y.zeroPaths()
	if s.parent == nil {
		return nil
	}

	sym := s.psym
	if sym.IsTerminal {
		return append(s.parent.syms0(), sym)
	}

	return append(s.parent.syms0(), sym.MinString()...)
}

// Syms0 returns an example of a string and a lookahead, if any, required to
// get to state s starting at state 0. If s is shifted into the lookahead is
// nil.
//
// Note: Invalid grammars and grammars with conflicts may have not all states
// reachable.
//
// To construct an example of a string for which the parser enters state s:
//
//	syms, la := s.Syms0()
//	if la != nil {
//		syms = append(syms, la)
//	}
//
func (s *State) Syms0() ([]*Symbol, *Symbol) {
	str := s.syms0()
	if s.parent == nil {
		return str, nil
	}

	if s.psym.IsTerminal {
		return str, nil
	}

	str0 := str
	var a []string
	for sym := range s.actions {
		str = append(str0, sym)
		if stop, _ := s.y.parse(s.id, func() *Symbol {
			if len(str) == 0 {
				return nil
			}

			r := str[0]
			str = str[1:]
			return r
		}); stop == s.id {
			a = append(a, sym.Name)
		}
	}
	if len(a) == 0 {
		//dbg("state %d.Syms0: failed to determine lookahead", s.id)
		return str0, nil
	}

	sort.Strings(a)
	return str0, s.y.Syms[a[0]]
}

// Reduce0 returns an example of a string required to reduce rule r in state s
// starting at state 0. If states s does not reduce rule r the string is empty.
//
// Note: Invalid grammars and grammars with conflicts may have not all states
// reachable and/or not all productions reducible.
func (s *State) Reduce0(r *Rule) []*Symbol {
	rn := r.RuleNum
	var sym *Symbol
	for s, acts := range s.actions {
		act := acts[0]
		if act.kind == 'r' && act.arg == rn {
			sym = s
			break
		}
	}
	if sym == nil {
		return nil
	}

	syms, _ := s.Syms0()
	return append(syms, sym)
}

// A special default symbol has Name "$default" and represents the default
// action.

// Symbol represents a terminal or non terminal symbol. A special end symbol
// has Name "$end" and represents the EOF token.
type Symbol struct {
	Associativity    int       // One of the assoc* constants.
	IsLeftRecursive  bool      // S: S ... ;
	IsRightRecursive bool      // S: ... S ;
	IsTerminal       bool      // Whether this is a terminal symbol.
	Name             string    // Textual value of the symbol, for example "IDENT" or "';'".
	Pos              token.Pos // Position where the symbol was firstly introduced.
	Precedence       int       // -1 of no precedence assigned.
	Rules            []*Rule   // Productions associated with this symbol.
	Type             string    // For example "int", "float64" or "foo", but possibly also "".
	Value            int       // Numeric value of the symbol.
	derivesE         bool      // Non terminal sym derives ε.
	derivesEValid    bool      //
	first1           symSet    //
	firstValid       bool      //
	follow           symSet    //
	id               int       // Index into y.syms
	minStr           []*Symbol //
	minStrOk         bool      //
}

func (s *Symbol) derivesEmpty(m map[*Symbol]bool) bool {
	if m[s] {
		return false
	}

	m[s] = true
	if s.IsTerminal {
		return false
	}

	if s.derivesEValid {
		return s.derivesE
	}

nextRule:
	for _, rule := range s.Rules {
		if len(rule.Components) == 0 {
			s.derivesE = true
			s.derivesEValid = true
			return true
		}

		for _, sym := range rule.syms {
			if !sym.derivesEmpty(m) {
				continue nextRule
			}
		}

		s.derivesE = true
		s.derivesEValid = true
		return true
	}
	s.derivesE = false
	s.derivesEValid = true
	return false
}

// DerivesEmpty returns whether s derives ε.
func (s *Symbol) DerivesEmpty() bool {
	return s.derivesEmpty(map[*Symbol]bool{})
}

// - dragon 4.4
// - http://www.cs.virginia.edu/~cs415/reading/FirstFollowLL.pdf
func (s *Symbol) first(y *y) (r symSet) {
	if s.firstValid {
		return s.first1
	}

	s.firstValid = true
	r = y.newSymSet(-1)
	for _, rule := range s.Rules {
		if len(rule.Components) == 0 {
			r.addEmpty()
			break
		}
	}
	s.first1 = r
	defer func() {
		s.first1 = r
	}()

	if s.IsTerminal {
		return y.newSymSet(s.id)
	}

	if s == y.emptySym {
		return y.newSymSet(s.id)
	}

nextRule:
	for _, rule := range s.Rules {
		for _, sym := range rule.syms {
			t := sym.first(y)
			r.add(t, false)
			if !t.hasEmpty() {
				continue nextRule
			}
		}
		r.addEmpty()
	}
	return r
}

// MinString returns an example of a string of symbols which can be reduced to
// s.  If s is a terminal symbol the result is s. If the only way to express
// some non terminal s includes s itself then nil is returned (and the grammar
// is invalid).
func (s *Symbol) MinString() (r []*Symbol) {
	r, _ = s.minString(nil)
	return r
}

func (s *Symbol) minString(m map[*Symbol]int) (r []*Symbol, ok bool) {
	//dbg("%s.minString ENTER", s)
	//defer func() { dbg("--------------> %s.minString returns %v, %v", s, r, ok) }()
	if str := s.minStr; str != nil {
		return str, s.minStrOk
	}

	defer func() {
		s.minStr = r
		s.minStrOk = ok
	}()

	if s.IsTerminal {
		return []*Symbol{s}, true
	}

	if s.DerivesEmpty() {
		return []*Symbol{}, true
	}

	if m[s] != 0 {
		return nil, false
	}

	if m == nil {
		m = map[*Symbol]int{}
	}
	m[s]++
	defer func() { m[s]-- }()
	var best []*Symbol
nextRule:
	for _, rule := range s.Rules {
		var current []*Symbol
		for _, sym := range rule.syms {
			str, ok := sym.minString(m)
			if !ok {
				continue nextRule
			}

			current = append(current, str...)
		}
		ok = true
		switch {
		case best == nil:
			best = current
		case len(current) < len(best):
			best = current
		case len(current) == len(best):
			for i, a := range current {
				b := best[i]
				if a.Name > b.Name {
					break
				}
				if a.Name < b.Name {
					best = current
					break
				}
			}
		}
	}
	return best, ok
}

// String implements fmt.Stringer.
func (s *Symbol) String() string {
	if s != nil {
		return s.Name
	}

	return "<nil>"
}

// XError describes the parser state for an error by example. See [1].
type XError struct {
	Stack     []int   // Parser states of the error event. TOS is Stack[len(Stack)-1].
	Lookahead *Symbol // LA of the error event. Nil if LA is the reserved error symbol.
	Msg       string  // Textual representation of the error condition.
}
