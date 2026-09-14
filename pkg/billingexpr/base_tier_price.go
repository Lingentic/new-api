package billingexpr

import (
	"fmt"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

const tokensPerMillion = 1_000_000

// BaseTierUnitPrices returns the per-million-token prices declared by the base tier.
// It falls back to the first tier when the expression has no explicitly named base tier.
// Cache reads fall back to the input price when the selected expression does not price cr.
func BaseTierUnitPrices(expression string) (input, output, cache float64, ok bool, err error) {
	_, body := ParseExprVersion(expression)
	if requestRules := strings.Index(body, "|||"); requestRules >= 0 {
		body = body[:requestRules]
	}
	tree, err := parser.Parse(body)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("parse billing expression: %w", err)
	}

	var firstTierExpression string
	var baseExpression string
	ast.Find(tree.Node, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallNode)
		if !isCall || len(call.Arguments) != 2 || call.Callee.String() != "tier" {
			return true
		}
		name, isName := call.Arguments[0].(*ast.StringNode)
		if !isName {
			return true
		}
		if firstTierExpression == "" {
			firstTierExpression = call.Arguments[1].String()
		}
		if name.Value == "base" && baseExpression == "" {
			baseExpression = call.Arguments[1].String()
		}
		return true
	})
	if baseExpression == "" {
		baseExpression = firstTierExpression
	}
	if baseExpression == "" {
		return 0, 0, 0, false, nil
	}

	input, err = evaluateUnitPrice(baseExpression, TokenParams{P: tokensPerMillion, Len: tokensPerMillion})
	if err != nil {
		return 0, 0, 0, false, err
	}
	output, err = evaluateUnitPrice(baseExpression, TokenParams{C: tokensPerMillion})
	if err != nil {
		return 0, 0, 0, false, err
	}
	if UsedVars(baseExpression)["cr"] {
		cache, err = evaluateUnitPrice(baseExpression, TokenParams{CR: tokensPerMillion})
		if err != nil {
			return 0, 0, 0, false, err
		}
	} else {
		cache = input
	}
	return input, output, cache, true, nil
}

func evaluateUnitPrice(expression string, params TokenParams) (float64, error) {
	price, _, err := RunExpr(expression, params)
	if err != nil {
		return 0, fmt.Errorf("evaluate base tier unit price: %w", err)
	}
	return price / tokensPerMillion, nil
}
