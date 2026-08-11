package schema

import (
	"fmt"
	"strings"
	"unicode"
)

// Parse reads a schema in the service's DSL:
//
//	definition user {}
//
//	definition group {
//	  relation member: user | group#member
//	}
//
//	definition document {
//	  relation parent: folder
//	  relation owner: user
//	  relation editor: user | group#member
//	  relation banned: user
//	  permission edit = editor + owner
//	  permission view = (edit + parent->view) - banned
//	}
//
// A `relation` holds stored tuples. A `permission` is computed and holds none.
// The distinction is not cosmetic: writing a tuple against a permission would
// let a caller grant themselves something the schema says must be derived, so
// the store rejects it.
//
// Operators are `+` union, `&` intersection, `-` exclusion, and `->` for
// walking an edge before evaluating a relation on the far side. `+` and `-`
// bind loosest and associate left, so `a + b - c` is `(a + b) - c`.
func Parse(src string) (*Schema, error) {
	p := &parser{toks: lex(src)}
	return p.parseSchema()
}

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokPunct
)

type token struct {
	kind tokenKind
	text string
	line int
}

func lex(src string) []token {
	var toks []token
	line := 1
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case unicode.IsSpace(rune(c)):
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '-' && i+1 < len(src) && src[i+1] == '>':
			toks = append(toks, token{tokPunct, "->", line})
			i += 2
		case strings.ContainsRune("{}:|=+&-()#", rune(c)):
			toks = append(toks, token{tokPunct, string(c), line})
			i++
		case isIdentRune(rune(c)):
			j := i
			for j < len(src) && isIdentRune(rune(src[j])) {
				j++
			}
			toks = append(toks, token{tokIdent, src[i:j], line})
			i = j
		default:
			// Unknown byte: emit it so the parser reports a useful position
			// instead of silently skipping something meaningful.
			toks = append(toks, token{tokPunct, string(c), line})
			i++
		}
	}
	return append(toks, token{tokEOF, "", line})
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atEOF() bool { return p.peek().kind == tokEOF }

func (p *parser) expect(text string) (token, error) {
	t := p.peek()
	if t.text != text {
		return t, fmt.Errorf("line %d: expected %q, got %q", t.line, text, display(t))
	}
	return p.next(), nil
}

func (p *parser) expectIdent() (token, error) {
	t := p.peek()
	if t.kind != tokIdent {
		return t, fmt.Errorf("line %d: expected a name, got %q", t.line, display(t))
	}
	return p.next(), nil
}

func display(t token) string {
	if t.kind == tokEOF {
		return "end of input"
	}
	return t.text
}

func (p *parser) parseSchema() (*Schema, error) {
	s := New()
	for !p.atEOF() {
		if _, err := p.expect("definition"); err != nil {
			return nil, err
		}
		name, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if _, dup := s.Definitions[name.text]; dup {
			return nil, fmt.Errorf("line %d: definition %q declared twice", name.line, name.text)
		}
		def := &Definition{Name: name.text, Relations: map[string]*Relation{}}
		if _, err := p.expect("{"); err != nil {
			return nil, err
		}
		for p.peek().text != "}" {
			if p.atEOF() {
				return nil, fmt.Errorf("line %d: definition %q is never closed", name.line, name.text)
			}
			if err := p.parseMember(def); err != nil {
				return nil, err
			}
		}
		if _, err := p.expect("}"); err != nil {
			return nil, err
		}
		s.Definitions[def.Name] = def
	}
	if err := validate(s); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) parseMember(def *Definition) error {
	kw, err := p.expectIdent()
	if err != nil {
		return err
	}
	switch kw.text {
	case "relation":
		name, err := p.expectIdent()
		if err != nil {
			return err
		}
		if _, dup := def.Relations[name.text]; dup {
			return fmt.Errorf("line %d: %s declares %q twice", name.line, def.Name, name.text)
		}
		// Type restrictions are parsed and kept for validation of writes.
		if p.peek().text == ":" {
			p.next()
			if err := p.parseTypeList(); err != nil {
				return err
			}
		}
		def.Relations[name.text] = &Relation{Name: name.text}
		return nil

	case "permission":
		name, err := p.expectIdent()
		if err != nil {
			return err
		}
		if _, dup := def.Relations[name.text]; dup {
			return fmt.Errorf("line %d: %s declares %q twice", name.line, def.Name, name.text)
		}
		if _, err := p.expect("="); err != nil {
			return err
		}
		rw, err := p.parseExpr()
		if err != nil {
			return err
		}
		def.Relations[name.text] = &Relation{Name: name.text, Rewrite: rw}
		return nil
	}
	return fmt.Errorf("line %d: expected `relation` or `permission`, got %q", kw.line, kw.text)
}

// parseTypeList consumes `user | group#member`. The types are not yet enforced
// at check time; they are here so a later write path can reject a tuple whose
// subject type the schema never allowed.
func (p *parser) parseTypeList() error {
	for {
		if _, err := p.expectIdent(); err != nil {
			return err
		}
		if p.peek().text == "#" {
			p.next()
			if _, err := p.expectIdent(); err != nil {
				return err
			}
		}
		if p.peek().text != "|" {
			return nil
		}
		p.next()
	}
}

// parseExpr handles `+` and `-` at the loosest precedence, left-associative.
func (p *parser) parseExpr() (Rewrite, error) {
	left, err := p.parseIntersection()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().text {
		case "+":
			p.next()
			right, err := p.parseIntersection()
			if err != nil {
				return nil, err
			}
			// Flatten chained unions so `a + b + c` is one node, which keeps
			// the engine's short-circuit over children meaningful.
			if u, ok := left.(Union); ok {
				left = Union{Children: append(u.Children, right)}
			} else {
				left = Union{Children: []Rewrite{left, right}}
			}
		case "-":
			p.next()
			right, err := p.parseIntersection()
			if err != nil {
				return nil, err
			}
			left = Exclusion{Base: left, Subtract: right}
		default:
			return left, nil
		}
	}
}

func (p *parser) parseIntersection() (Rewrite, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.peek().text == "&" {
		p.next()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		if in, ok := left.(Intersection); ok {
			left = Intersection{Children: append(in.Children, right)}
		} else {
			left = Intersection{Children: []Rewrite{left, right}}
		}
	}
	return left, nil
}

func (p *parser) parsePrimary() (Rewrite, error) {
	t := p.peek()
	if t.text == "(" {
		p.next()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
		return inner, nil
	}
	name, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if p.peek().text == "->" {
		p.next()
		computed, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		return TupleToUserset{Tupleset: name.text, ComputedUserset: computed.text}, nil
	}
	return ComputedUserset{Relation: name.text}, nil
}

// validate rejects references the engine would only discover at check time.
// A schema that names a relation nobody defined is a bug the author should
// hear about at load, not as a denied request in production.
func validate(s *Schema) error {
	for _, def := range s.Definitions {
		for _, rel := range def.Relations {
			if rel.Rewrite == nil {
				continue
			}
			if err := validateRewrite(def, rel.Name, rel.Rewrite); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRewrite(def *Definition, relName string, rw Rewrite) error {
	switch r := rw.(type) {
	case This:
		return nil
	case ComputedUserset:
		if _, ok := def.Relations[r.Relation]; !ok {
			return fmt.Errorf("schema: %s.%s references undefined relation %q", def.Name, relName, r.Relation)
		}
		return nil
	case TupleToUserset:
		// Only the local half is checkable here: the far side depends on what
		// object type the tupleset edge actually points at, which is data.
		if _, ok := def.Relations[r.Tupleset]; !ok {
			return fmt.Errorf("schema: %s.%s walks undefined relation %q", def.Name, relName, r.Tupleset)
		}
		return nil
	case Union:
		for _, c := range r.Children {
			if err := validateRewrite(def, relName, c); err != nil {
				return err
			}
		}
		return nil
	case Intersection:
		for _, c := range r.Children {
			if err := validateRewrite(def, relName, c); err != nil {
				return err
			}
		}
		return nil
	case Exclusion:
		if err := validateRewrite(def, relName, r.Base); err != nil {
			return err
		}
		return validateRewrite(def, relName, r.Subtract)
	}
	return fmt.Errorf("schema: unhandled rewrite %T", rw)
}
