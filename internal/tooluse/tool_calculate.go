package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser"
)

const (
	calculateMaxExpressionRunes = 256
	calculateMaxNodes           = 64
	calculateMaxDepth           = 32
)

// calculateFunc is one allowlisted function with its inclusive arity range.
type calculateFunc struct {
	min, max int
	fn       func(args []float64) float64
}

func unary(fn func(float64) float64) calculateFunc {
	return calculateFunc{min: 1, max: 1, fn: func(a []float64) float64 { return fn(a[0]) }}
}

func binary(fn func(float64, float64) float64) calculateFunc {
	return calculateFunc{min: 2, max: 2, fn: func(a []float64) float64 { return fn(a[0], a[1]) }}
}

var calculateFunctions = map[string]calculateFunc{
	"sqrt":  unary(math.Sqrt),
	"cbrt":  unary(math.Cbrt),
	"abs":   unary(math.Abs),
	"exp":   unary(math.Exp),
	"ln":    unary(math.Log),
	"log10": unary(math.Log10),
	"log2":  unary(math.Log2),
	"sin":   unary(math.Sin),
	"cos":   unary(math.Cos),
	"tan":   unary(math.Tan),
	"asin":  unary(math.Asin),
	"acos":  unary(math.Acos),
	"atan":  unary(math.Atan),
	"sinh":  unary(math.Sinh),
	"cosh":  unary(math.Cosh),
	"tanh":  unary(math.Tanh),
	"floor": unary(math.Floor),
	"ceil":  unary(math.Ceil),
	"round": unary(math.Round),
	"trunc": unary(math.Trunc),
	"atan2": binary(math.Atan2),
	"hypot": binary(math.Hypot),
	"pow":   binary(math.Pow),
	"mod":   binary(math.Mod),
	"min":   {min: 2, max: 4, fn: func(a []float64) float64 { return extremum(a, math.Min) }},
	"max":   {min: 2, max: 4, fn: func(a []float64) float64 { return extremum(a, math.Max) }},
}

var calculateConstants = map[string]float64{
	"pi": math.Pi,
	"e":  math.E,
}

func extremum(values []float64, pick func(a, b float64) float64) float64 {
	result := values[0]
	for _, v := range values[1:] {
		result = pick(result, v)
	}

	return result
}

var (
	errCalcTooComplex = errors.New("too complex")
	errCalcNonFinite  = errors.New("non-finite")
)

type calcUnsupportedError struct{ what string }

func (e *calcUnsupportedError) Error() string { return e.what }

type calcArityError struct {
	name     string
	min, max int
}

func (e *calcArityError) Error() string { return e.name }

func calculateError(code, message, hint string) toolResult {
	return ErrorResult(NewToolError(code, ErrorArguments, message, true, hint))
}

func calculateHandler(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Expression string `json:"expression"`
	}
	if !decodeToolJSON(args, &payload) {
		return toolResult{}, invalidArguments("Provide an object with a single expression string.")
	}

	result := evaluateCalculation(payload.Expression)
	logToolResult(callCtx, result)

	return result, nil
}

// evaluateCalculation parses with the expr parser only and evaluates the
// validated tree itself: the tree that was checked is the tree that runs. No
// expr compiler, optimizer, VM or builtin is involved.
func evaluateCalculation(expression string) toolResult {
	if utf8.RuneCountInString(expression) == 0 || len(expression) > calculateMaxExpressionRunes*utf8.UTFMax {
		return calculateError("invalid_expression", "expression must be a non-empty arithmetic expression.", "Provide an expression such as sqrt(2) * 3^2.")
	}
	if utf8.RuneCountInString(expression) > calculateMaxExpressionRunes {
		return calculateError("expression_too_complex", fmt.Sprintf("expression exceeds %d characters.", calculateMaxExpressionRunes), "Split the calculation into smaller steps.")
	}

	tree, err := parser.Parse(expression)
	if err != nil {
		var parseErr *file.Error
		if errors.As(err, &parseErr) {
			return calculateError("invalid_expression", fmt.Sprintf("expression is not valid (line %d, column %d).", parseErr.Line+1, parseErr.Column+1), "Use numbers, + - * / ^, parentheses, constants and supported functions.")
		}

		return calculateError("invalid_expression", "expression is not valid.", "Use numbers, + - * / ^, parentheses, constants and supported functions.")
	}

	counter := &nodeCounter{}
	ast.Walk(&tree.Node, counter)
	if counter.count > calculateMaxNodes {
		return calculateError("expression_too_complex", fmt.Sprintf("expression has more than %d elements.", calculateMaxNodes), "Split the calculation into smaller steps.")
	}

	value, err := evalCalcNode(tree.Node, 0)
	if err != nil {
		return calculateFailure(err)
	}

	return toolResult{
		Status:  "ok",
		Summary: "Expression evaluated",
		Data: map[string]any{
			"value":     value,
			"formatted": strconv.FormatFloat(value, 'g', -1, 64),
		},
	}
}

func calculateFailure(err error) toolResult {
	var unsupported *calcUnsupportedError
	var arity *calcArityError
	switch {
	case errors.As(err, &unsupported):
		return calculateError("unsupported_operation", unsupported.what+" is not supported.", "Use numbers, + - * / ^, parentheses, constants pi and e, and the documented functions only.")
	case errors.As(err, &arity):
		want := strconv.Itoa(arity.min)
		if arity.max != arity.min {
			want = fmt.Sprintf("%d to %d", arity.min, arity.max)
		}

		return calculateError("wrong_arity", fmt.Sprintf("%s takes %s argument(s).", arity.name, want), "Correct the number of arguments.")
	case errors.Is(err, errCalcTooComplex):
		return calculateError("expression_too_complex", "expression is nested too deeply.", "Split the calculation into smaller steps.")
	case errors.Is(err, errCalcNonFinite):
		return calculateError("non_finite_result", "the calculation has no finite real result (division by zero, domain error or overflow).", "Check divisors and function domains, or reduce magnitudes.")
	default:
		return calculateError("invalid_expression", "expression could not be evaluated.", "Check the expression.")
	}
}

type nodeCounter struct{ count int }

func (c *nodeCounter) Visit(_ *ast.Node) {
	c.count++
}

func finiteOrError(value float64) (float64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errCalcNonFinite
	}

	return value, nil
}

func evalCalcNode(node ast.Node, depth int) (float64, error) {
	if depth > calculateMaxDepth {
		return 0, errCalcTooComplex
	}

	switch n := node.(type) {
	case *ast.IntegerNode:
		return float64(n.Value), nil
	case *ast.FloatNode:
		return finiteOrError(n.Value)
	case *ast.IdentifierNode:
		if value, ok := calculateConstants[n.Value]; ok {
			return value, nil
		}

		return 0, &calcUnsupportedError{what: "identifier " + quoteBounded(n.Value)}
	case *ast.UnaryNode:
		return evalCalcUnary(n, depth)
	case *ast.BinaryNode:
		return evalCalcBinary(n, depth)
	case *ast.CallNode:
		callee, ok := n.Callee.(*ast.IdentifierNode)
		if !ok {
			return 0, &calcUnsupportedError{what: "this call form"}
		}

		return evalCalcCall(callee.Value, n.Arguments, depth)
	case *ast.BuiltinNode:
		return evalCalcCall(n.Name, n.Arguments, depth)
	default:
		return 0, &calcUnsupportedError{what: nodeKind(node)}
	}
}

func evalCalcUnary(n *ast.UnaryNode, depth int) (float64, error) {
	if n.Operator != "-" && n.Operator != "+" {
		return 0, &calcUnsupportedError{what: "operator " + quoteBounded(n.Operator)}
	}
	value, err := evalCalcNode(n.Node, depth+1)
	if err != nil {
		return 0, err
	}
	if n.Operator == "-" {
		value = -value
	}

	return value, nil
}

func evalCalcBinary(n *ast.BinaryNode, depth int) (float64, error) {
	switch n.Operator {
	case "+", "-", "*", "/", "^", "**":
	default:
		return 0, &calcUnsupportedError{what: "operator " + quoteBounded(n.Operator)}
	}
	left, err := evalCalcNode(n.Left, depth+1)
	if err != nil {
		return 0, err
	}
	right, err := evalCalcNode(n.Right, depth+1)
	if err != nil {
		return 0, err
	}

	var value float64
	switch n.Operator {
	case "+":
		value = left + right
	case "-":
		value = left - right
	case "*":
		value = left * right
	case "/":
		value = left / right
	default:
		value = math.Pow(left, right)
	}

	return finiteOrError(value)
}

func evalCalcCall(name string, arguments []ast.Node, depth int) (float64, error) {
	spec, ok := calculateFunctions[name]
	if !ok {
		return 0, &calcUnsupportedError{what: "function " + quoteBounded(name)}
	}
	if len(arguments) < spec.min || len(arguments) > spec.max {
		return 0, &calcArityError{name: name, min: spec.min, max: spec.max}
	}
	values := make([]float64, len(arguments))
	for i, argument := range arguments {
		value, err := evalCalcNode(argument, depth+1)
		if err != nil {
			return 0, err
		}
		values[i] = value
	}

	return finiteOrError(spec.fn(values))
}

func nodeKind(node ast.Node) string {
	switch node.(type) {
	case *ast.StringNode:
		return "string literals"
	case *ast.BoolNode:
		return "boolean literals"
	case *ast.NilNode:
		return "nil"
	case *ast.MemberNode, *ast.ChainNode:
		return "member access"
	case *ast.SliceNode:
		return "slices and ranges"
	case *ast.ConditionalNode:
		return "conditional expressions"
	case *ast.ArrayNode, *ast.MapNode:
		return "collection literals"
	case *ast.PredicateNode, *ast.PointerNode:
		return "closures"
	default:
		return "this element"
	}
}

// quoteBounded renders a model-supplied token for an error message without
// letting it grow the result.
func quoteBounded(token string) string {
	return strconv.Quote(truncateUTF8(token, 24))
}
