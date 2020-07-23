package promlinter

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"
)

var (
	metricsType       map[string]dto.MetricType
	constMetricArgNum map[string]int
	validOptsFields   map[string]bool
)

func init() {
	metricsType = map[string]dto.MetricType{
		"NewCounter":      dto.MetricType_COUNTER,
		"NewCounterVec":   dto.MetricType_COUNTER,
		"NewGauge":        dto.MetricType_GAUGE,
		"NewGaugeVec":     dto.MetricType_GAUGE,
		"NewHistogram":    dto.MetricType_HISTOGRAM,
		"NewHistogramVec": dto.MetricType_HISTOGRAM,
		"NewSummary":      dto.MetricType_SUMMARY,
		"NewSummaryVec":   dto.MetricType_SUMMARY,
	}

	constMetricArgNum = map[string]int{
		"MustNewConstMetric": 3,
		"MustNewHistogram":   4,
		"MustNewSummary":     4,
		"NewLazyConstMetric": 3,
	}

	// Doesn't contain ConstLabels since we don't need this field here.
	validOptsFields = map[string]bool{
		"Name":      true,
		"Namespace": true,
		"Subsystem": true,
		"Help":      true,
	}
}

type PosRange struct {
	Start token.Position
	End   token.Position
}

// Issue contains metric name, error text and metric position.
type Issue struct {
	Pos    PosRange
	Metric string
	Text   string
}

type visitor struct {
	fs      *token.FileSet
	metrics map[*dto.MetricFamily]PosRange
	issues  []Issue
	strict  bool
}

type opt struct {
	namespace string
	subsystem string
	name      string
}

func Run(fs *token.FileSet, files []*ast.File, strict bool) []Issue {
	v := &visitor{
		fs:      fs,
		metrics: make(map[*dto.MetricFamily]PosRange, 0),
		issues:  make([]Issue, 0),
		strict:  strict,
	}

	for _, file := range files {
		ast.Walk(v, file)
	}

	// lint metrics
	for metric := range v.metrics {
		problems, err := promlint.NewWithMetricFamilies([]*dto.MetricFamily{metric}).Lint()
		if err != nil {
			panic(err)
		}

		for _, p := range problems {
			v.issues = append(v.issues, Issue{
				Pos:    v.metrics[metric],
				Metric: p.Metric,
				Text:   p.Text,
			})
		}
	}

	sort.Slice(v.issues, func(i, j int) bool {
		return v.issues[i].Pos.Start.String() < v.issues[j].Pos.Start.String()
	})
	return v.issues
}

func (v *visitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return v
	}

	switch t := n.(type) {
	case *ast.CallExpr:
		return v.parseCallerExpr(t)

	case *ast.SendStmt:
		return v.parseSendMetricChanExpr(t)
	}

	return v
}

func (v *visitor) parseCallerExpr(call *ast.CallExpr) ast.Visitor {
	var (
		metricType dto.MetricType
		methodName string
		ok         bool
	)

	switch stmt := call.Fun.(type) {

	/*
		That's the case of setting alias . to client_golang/prometheus or promauto package.

			import . "github.com/prometheus/client_golang/prometheus"
			metric := NewCounter(CounterOpts{})
	*/
	case *ast.Ident:
		if stmt.Name == "NewCounterFunc" {
			return v.parseOpts(call.Args[0], dto.MetricType_COUNTER)
		}

		if stmt.Name == "NewGaugeFunc" {
			return v.parseOpts(call.Args[0], dto.MetricType_GAUGE)
		}

		if metricType, ok = metricsType[stmt.Name]; !ok {
			return v
		}
		methodName = stmt.Name

	/*
		This case covers the most of cases to initialize metrics.

			prometheus.NewCounter(CounterOpts{})

			promauto.With(nil).NewCounter(CounterOpts{})

			factory := promauto.With(nil)
			factory.NewCounter(CounterOpts{})

			prometheus.NewCounterFunc()
	*/
	case *ast.SelectorExpr:
		if stmt.Sel.Name == "NewCounterFunc" {
			return v.parseOpts(call.Args[0], dto.MetricType_COUNTER)
		}

		if stmt.Sel.Name == "NewGaugeFunc" {
			return v.parseOpts(call.Args[0], dto.MetricType_GAUGE)
		}

		if metricType, ok = metricsType[stmt.Sel.Name]; !ok {
			return v
		}
		methodName = stmt.Sel.Name

	default:
		return v
	}

	argNum := 1
	if strings.HasSuffix(methodName, "Vec") {
		argNum = 2
	}
	// The methods used to initialize metrics should have at least one arg.
	if len(call.Args) < argNum && v.strict {
		v.issues = append(v.issues, Issue{
			Pos: PosRange{
				Start: v.fs.Position(call.Pos()),
				End:   v.fs.Position(call.End()),
			},
			Metric: "",
			Text:   fmt.Sprintf("%s should have at least %d arguments", methodName, argNum),
		})
		return v
	}

	return v.parseOpts(call.Args[0], metricType)
}

func (v *visitor) parseOpts(optArg ast.Node, metricType dto.MetricType) ast.Visitor {
	opts, help := v.parseOptsExpr(optArg)
	if opts == nil {
		return v
	}
	currentMetric := dto.MetricFamily{
		Type: &metricType,
		Help: help,
	}

	metricName := prometheus.BuildFQName(opts.namespace, opts.subsystem, opts.name)
	currentMetric.Name = &metricName

	v.metrics[&currentMetric] = PosRange{
		Start: v.fs.Position(optArg.Pos()),
		End:   v.fs.Position(optArg.End()),
	}
	return v
}

func (v *visitor) parseSendMetricChanExpr(chExpr *ast.SendStmt) ast.Visitor {
	var (
		ok             bool
		requiredArgNum int
		methodName     string
		metricType     dto.MetricType
	)

	call, ok := chExpr.Value.(*ast.CallExpr)
	if !ok {
		return v
	}

	switch stmt := call.Fun.(type) {
	case *ast.Ident:
		if requiredArgNum, ok = constMetricArgNum[stmt.Name]; !ok {
			return v
		}
		methodName = stmt.Name

	case *ast.SelectorExpr:
		if requiredArgNum, ok = constMetricArgNum[stmt.Sel.Name]; !ok {
			return v
		}
		methodName = stmt.Sel.Name
	}

	if len(call.Args) < requiredArgNum && v.strict {
		v.issues = append(v.issues, Issue{
			Pos: PosRange{
				Start: v.fs.Position(call.Pos()),
				End:   v.fs.Position(call.End()),
			},
			Metric: "",
			Text:   fmt.Sprintf("%s should have at least %d arguments", methodName, requiredArgNum),
		})
		return v
	}

	name, help := v.parseConstMetricOptsExpr(call.Args[0])
	if name == nil {
		return v
	}

	metric := &dto.MetricFamily{
		Name: name,
		Help: help,
	}
	switch methodName {
	case "MustNewConstMetric", "NewLazyConstMetric":
		switch t := call.Args[1].(type) {
		case *ast.Ident:
			metric.Type = getConstMetricType(t.Name)
		case *ast.SelectorExpr:
			metric.Type = getConstMetricType(t.Sel.Name)
		}

	case "MustNewHistogram":
		metricType = dto.MetricType_HISTOGRAM
		metric.Type = &metricType
	case "MustNewSummary":
		metricType = dto.MetricType_SUMMARY
		metric.Type = &metricType
	}

	v.metrics[metric] = PosRange{
		Start: v.fs.Position(call.Pos()),
		End:   v.fs.Position(call.End()),
	}
	return v
}

func (v *visitor) parseOptsExpr(n ast.Node) (*opt, *string) {
	switch stmt := n.(type) {
	case *ast.CompositeLit:
		return v.parseCompositeOpts(stmt)

	case *ast.Ident:
		if stmt.Obj != nil {
			if decl, ok := stmt.Obj.Decl.(*ast.AssignStmt); ok && len(decl.Rhs) > 0 {
				if t, ok := decl.Rhs[0].(*ast.CompositeLit); ok {
					return v.parseCompositeOpts(t)
				}
			}
		}

	case *ast.UnaryExpr:
		return v.parseOptsExpr(stmt.X)
	}

	return nil, nil
}

func (v *visitor) parseCompositeOpts(stmt *ast.CompositeLit) (*opt, *string) {
	metricOption := &opt{}
	var help *string
	for _, elt := range stmt.Elts {
		kvExpr, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		object, ok := kvExpr.Key.(*ast.Ident)
		if !ok {
			continue
		}

		if _, ok := validOptsFields[object.Name]; !ok {
			continue
		}

		// If failed to parse field value, stop parsing.
		stringLiteral, ok := v.parseValue(object.Name, kvExpr.Value)
		if !ok {
			return nil, nil
		}

		switch object.Name {
		case "Namespace":
			metricOption.namespace = stringLiteral
		case "Subsystem":
			metricOption.subsystem = stringLiteral
		case "Name":
			metricOption.name = stringLiteral
		case "Help":
			help = &stringLiteral
		}
	}

	return metricOption, help
}

func (v *visitor) parseValue(object string, n ast.Node) (string, bool) {
	switch t := n.(type) {

	// make sure it is string literal value
	case *ast.BasicLit:
		if t.Kind == token.STRING {
			return mustUnquote(t.Value), true
		}

		return "", false

	case *ast.Ident:
		if t.Obj == nil {
			return "", false
		}

		if vs, ok := t.Obj.Decl.(*ast.ValueSpec); ok {
			return v.parseValue(object, vs)
		}

	case *ast.ValueSpec:
		if len(t.Values) == 0 {
			return "", false
		}
		return v.parseValue(object, t.Values[0])

	// For binary expr, we only support adding two strings like `foo` + `bar`.
	case *ast.BinaryExpr:
		if t.Op == token.ADD {
			x, ok := v.parseValue(object, t.X)
			if !ok {
				return "", false
			}

			y, ok := v.parseValue(object, t.Y)
			if !ok {
				return "", false
			}

			return x + y, true
		}

	// We can only cover some basic cases here
	case *ast.CallExpr:
		return v.parseValueCallExpr(object, t)

	default:
		if v.strict {
			v.issues = append(v.issues, Issue{
				Pos: PosRange{
					Start: v.fs.Position(n.Pos()),
					End:   v.fs.Position(n.End()),
				},
				Metric: "",
				Text:   fmt.Sprintf("parsing %s with type %T is not supported", object, t),
			})
		}
	}

	return "", false
}

func (v *visitor) parseValueCallExpr(object string, call *ast.CallExpr) (string, bool) {
	var (
		methodName string
		namespace  string
		subsystem  string
		name       string
		ok         bool
	)
	switch expr := call.Fun.(type) {
	case *ast.SelectorExpr:
		methodName = expr.Sel.Name
	case *ast.Ident:
		methodName = expr.Name
	default:
		return "", false
	}

	if methodName == "BuildFQName" && len(call.Args) == 3 {
		namespace, ok = v.parseValue("namespace", call.Args[0])
		if !ok {
			return "", false
		}
		subsystem, ok = v.parseValue("subsystem", call.Args[1])
		if !ok {
			return "", false
		}
		name, ok = v.parseValue("name", call.Args[2])
		if !ok {
			return "", false
		}
		return prometheus.BuildFQName(namespace, subsystem, name), true
	}

	if v.strict {
		v.issues = append(v.issues, Issue{
			Pos: PosRange{
				Start: v.fs.Position(call.Pos()),
				End:   v.fs.Position(call.End()),
			},
			Metric: "",
			Text:   fmt.Sprintf("parsing %s with Method %s is not supported", object, methodName),
		})
	}

	return "", false
}

func (v *visitor) parseConstMetricOptsExpr(n ast.Node) (*string, *string) {
	switch stmt := n.(type) {
	case *ast.CallExpr:
		return v.parseNewDescCallExpr(stmt)

	case *ast.Ident:
		if stmt.Obj != nil {
			switch t := stmt.Obj.Decl.(type) {
			case *ast.AssignStmt:
				if len(t.Rhs) > 0 {
					if call, ok := t.Rhs[0].(*ast.CallExpr); ok {
						return v.parseNewDescCallExpr(call)
					}
				}
			case *ast.ValueSpec:
				if len(t.Values) > 0 {
					if call, ok := t.Values[0].(*ast.CallExpr); ok {
						return v.parseNewDescCallExpr(call)
					}
				}
			}

			if v.strict {
				v.issues = append(v.issues, Issue{
					Pos: PosRange{
						Start: v.fs.Position(n.Pos()),
						End:   v.fs.Position(n.End()),
					},
					Metric: "",
					Text:   fmt.Sprintf("parsing desc of type %T is not supported", stmt.Obj.Decl),
				})
			}
		}
	}

	return nil, nil
}

func (v *visitor) parseNewDescCallExpr(call *ast.CallExpr) (*string, *string) {
	var (
		help string
		name string
		ok   bool
	)

	// k8s.io/component-base/metrics.NewDesc has 6 args
	// while prometheus.NewDesc has 4 args
	if len(call.Args) < 4 && v.strict {
		v.issues = append(v.issues, Issue{
			Pos: PosRange{
				Start: v.fs.Position(call.Pos()),
				End:   v.fs.Position(call.End()),
			},
			Metric: "",
			Text:   "NewDesc should have at least 4 args",
		})
		return nil, nil
	}

	name, ok = v.parseValue("fqName", call.Args[0])
	if !ok {
		return nil, nil
	}
	help, ok = v.parseValue("help", call.Args[1])
	if !ok {
		return nil, nil
	}

	return &name, &help
}

func mustUnquote(str string) string {
	stringLiteral, err := strconv.Unquote(str)
	if err != nil {
		panic(err)
	}

	return stringLiteral
}

func getConstMetricType(name string) *dto.MetricType {
	metricType := dto.MetricType_UNTYPED
	if name == "CounterValue" {
		metricType = dto.MetricType_COUNTER
	} else if name == "GaugeValue" {
		metricType = dto.MetricType_GAUGE
	}

	return &metricType
}
