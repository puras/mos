package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode"

	"hegq.net/mos/tool"
)

// Calculator 计算数学表达式（支持 + - * / % 与括号、一元负号）。
type Calculator struct{}

type calcParams struct {
	Expr string `json:"expr" mos:"desc=数学表达式，如 (1+2)*3"`
}

func (Calculator) Name() string { return "calculator" }
func (Calculator) Description() string {
	return "计算数学表达式，支持四则运算、括号与取模"
}
func (Calculator) Parameters() any { return &calcParams{} }
func (Calculator) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	v, err := evalExpr(p.Expr)
	if err != nil {
		return nil, err
	}
	return formatNumber(v), nil
}

// formatNumber 去除无意义的浮点尾缀（如 3.000 → 3）。
func formatNumber(v float64) string {
	s := fmt.Sprintf("%g", v)
	return s
}

// ---- 简易表达式求值器（递归下降） ----

type exprParser struct {
	input string
	pos   int
}

// evalExpr 求值数学表达式，返回结果。
func evalExpr(expr string) (float64, error) {
	p := &exprParser{input: expr}
	v, err := p.parseExpr()
	if err != nil {
		return 0, fmt.Errorf("表达式无效: %w", err)
	}
	p.skipSpace()
	if p.pos != len(p.input) {
		return 0, fmt.Errorf("表达式无效: 存在无法解析的内容 %q", p.input[p.pos:])
	}
	return v, nil
}

func (p *exprParser) skipSpace() {
	for p.pos < len(p.input) && unicode.IsSpace(rune(p.input[p.pos])) {
		p.pos++
	}
}

func (p *exprParser) peek() byte {
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *exprParser) parseExpr() (float64, error) {
	v, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		switch p.peek() {
		case '+':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			v += r
		case '-':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			v -= r
		default:
			return v, nil
		}
	}
}

func (p *exprParser) parseTerm() (float64, error) {
	v, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		switch p.peek() {
		case '*':
			p.pos++
			r, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			v *= r
		case '/':
			p.pos++
			r, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("除数不能为 0")
			}
			v /= r
		case '%':
			p.pos++
			r, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			v = float64(int(v) % int(r))
		default:
			return v, nil
		}
	}
}

func (p *exprParser) parseFactor() (float64, error) {
	p.skipSpace()
	switch p.peek() {
	case '(':
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.peek() != ')' {
			return 0, fmt.Errorf("缺少右括号")
		}
		p.pos++
		return v, nil
	case '-':
		p.pos++
		v, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		return -v, nil
	case '+':
		p.pos++
		return p.parseFactor()
	}
	return p.parseNumber()
}

func (p *exprParser) parseNumber() (float64, error) {
	start := p.pos
	dot := false
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if c == '.' && !dot {
			dot = true
			p.pos++
			continue
		}
		if c >= '0' && c <= '9' {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return 0, fmt.Errorf("期望数字，但遇到 %q", string(p.peek()))
	}
	var v float64
	if _, err := fmt.Sscanf(p.input[start:p.pos], "%g", &v); err != nil {
		return 0, fmt.Errorf("数字解析失败 %q", p.input[start:p.pos])
	}
	return v, nil
}

// 确保 calculator 实现 tool.Tool 接口。
var _ tool.Tool = Calculator{}
