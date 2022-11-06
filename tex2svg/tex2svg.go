// Package tex2svg provides the support for coverting the
// specified TeX input into a parsed SVG's HTML node.
//
// To work, we will first generate a TeX file and convert
// it into a PDF or DVI file using "pdflatex". Then we will
// convert the PDF file into the SVG file using "pdf2svg".
package tex2svg

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"text/template"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type option struct {
	fontSize    int
	template    string
	templateDir string
	fallable    bool

	precision        int
	controlPrecision int

	attrs map[string]string

	logger *log.Logger
}

// regexpLastError for matching the error pattern found
// in the latex execution log.
var regexpLatexError = regexp.MustCompilePOSIX("^! (.*)$")

// regexpDvisvgmDepth for matching depth information.
var regexpDvisvgmDepth = regexp.MustCompile("\\bdepth=([0-9.e-]+)pt")

// Option passed for controlling the SVG generation.
type Option func(*option)

// WithFontSize sets font size in TeX's pt unit.
func WithFontSize(pt int) Option {
	return func(option *option) {
		option.fontSize = pt
	}
}

// WithTemplateDir sets the template search directory.
//
// The template corresponding to specified node type will
// be looked up and used. Each template should accept in
// the template arguments FontSize and Code.
//
// By default, the executable's directory will be searched
// for templates. And DeX comes with some preset templates.
func WithTemplateDir(dir string) Option {
	return func(option *option) {
		option.template = ""
		option.templateDir = dir
	}
}

// WithTemplate sets the template forcefully.
//
// Setting this will force all kinds of generation to use
// the specified template instead. This is useful for
// debugging template generation dedicatedly.
func WithTemplate(template string) Option {
	return func(option *option) {
		option.template = template
		option.templateDir = ""
	}
}

// WithFallable specifies that the "latex.tex" will be
// used when a specified type is not present.
func WithFallable(fallable bool) Option {
	return func(option *option) {
		option.fallable = fallable
	}
}

func (option *option) compileTemplate(
	typ string, funcs template.FuncMap,
) (*template.Template, error) {
	content := option.template
	if content == "" {
		// We'll have to look for the specified template.
		templateDir := option.templateDir
		if templateDir == "" {
			exePath, err := os.Executable()
			if err != nil {
				return nil, errors.Wrap(
					err, "get executable path")
			}
			templateDir = filepath.Dir(exePath)
		}

		// Attempt to open the specified template.
		b, err := ioutil.ReadFile(filepath.Join(templateDir, typ+".tex"))
		if err != nil {
			if os.IsNotExist(err) && option.fallable {
				typ = "latex"
				b, err = ioutil.ReadFile(filepath.Join(templateDir, typ+".tex"))
			}
			if err != nil {
				return nil, errors.Wrapf(
					err, "retrieve template %q", typ)
			}
		}
		content = string(b)
	}
	return template.New("").Funcs(funcs).Parse(content)
}

// WithPrecision updates the precision of the node points.
func WithPrecision(precision int) Option {
	return func(option *option) {
		option.precision = precision
	}
}

// WithControlPrecision updates the precision of the
// control points in path.
func WithControlPrecision(precision int) Option {
	return func(option *option) {
		option.controlPrecision = precision
	}
}

// WithAttributes specifies the attributes.
func WithAttributes(attrs map[string]string) Option {
	return func(option *option) {
		for key, value := range attrs {
			option.attrs[key] = value
		}
	}
}

// WithLogger sets a logger for printing out content.
func WithLogger(logger *log.Logger) Option {
	return func(option *option) {
		option.logger = logger
	}
}

// WithOptions aggregates a few options into a single option.
func WithOptions(opts ...Option) Option {
	return func(option *option) {
		for _, opt := range opts {
			opt(option)
		}
	}
}

// Result is the result of tex2svg generation.
type Result struct {
	Data     []byte
	Baseline float64
}

// Generate SVG using the provided LaTeX data.
func Generate(
	ctx context.Context, typ, code string, opts ...Option,
) (*Result, error) {
	var option option
	option.fontSize = 12
	option.precision = 4
	option.controlPrecision = 2
	option.attrs = make(map[string]string)
	option.logger = log.New()
	option.logger.SetOutput(ioutil.Discard)
	WithOptions(opts...)(&option)

	// Create the temporary directory for generated file.
	workDir, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, errors.Wrap(err, "create temp dir")
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	option.logger.Infof("workdir %q created", workDir)

	// Generate the TeX file as the ingredient.
	texPath := filepath.Join(workDir, "code.tex")
	texFile, err := os.Create(texPath)
	if err != nil {
		return nil, errors.Wrap(err, "create TeX file")
	}
	defer func() {
		if texFile != nil {
			_ = texFile.Close()
		}
	}()
	var data struct {
		FontSize int
		Code     string
	}
	data.FontSize = option.fontSize
	data.Code = code
	template, err := option.compileTemplate(typ, template.FuncMap{
		"attr": func(key string) string {
			return option.attrs[key]
		},
		"attrfmt": func(key, format string) string {
			if value, ok := option.attrs[key]; ok {
				return fmt.Sprintf(format, value)
			}
			return ""
		},
	})
	if err != nil {
		return nil, err
	}
	if err := template.Execute(texFile, &data); err != nil {
		return nil, errors.Wrap(err, "generate TeX")
	}
	_ = texFile.Close()
	texFile = nil

	// Read the generating file for debugging what's happening.
	if option.logger.IsLevelEnabled(log.DebugLevel) {
		if data, err := ioutil.ReadFile(texPath); err == nil {
			option.logger.Debugf("tex %q content: %s",
				texPath, string(data))
		}
	}

	// Execute the LaTeX generate command.
	latexExePath, err := exec.LookPath("latex")
	if err != nil {
		return nil, errors.Wrap(err, "lookup latex")
	}
	latexCmd := exec.CommandContext(ctx, latexExePath,
		"-interaction", "nonstopmode", "-halt-on-error", "code.tex")
	latexCmd.Dir = workDir
	option.logger.Infof("command %q execute", latexCmd)
	output, err := latexCmd.CombinedOutput()
	if err != nil {
		var targetErr *exec.ExitError
		if errors.As(err, &targetErr) {
			if option.logger.IsLevelEnabled(log.ErrorLevel) {
				if data, err := ioutil.ReadFile(texPath); err == nil {
					option.logger.Errorf("tex %q content: %s",
						texPath, string(data))
				}
			}
			option.logger.Errorf("tex %q generate error: %v",
				texPath, string(output))
			match := regexpLatexError.FindSubmatch(output)
			if len(match) > 1 {
				err = errors.New(string(match[1]))
			}
		}
		return nil, errors.Wrap(err, "exec latex")
	}

	// Execute the DVISVGM convert command.
	dvisvgmExePath, err := exec.LookPath("dvisvgm")
	if err != nil {
		return nil, errors.Wrap(err, "lookup dvisvgm")
	}
	dvisvgmCmd := exec.CommandContext(ctx, dvisvgmExePath,
		"--no-fonts", "--exact-bbox", "code.dvi")
	dvisvgmCmd.Dir = workDir
	option.logger.Infof("command %q execute", dvisvgmCmd)
	dvisvgmOutput, err := dvisvgmCmd.CombinedOutput()
	if err != nil {
		if len(dvisvgmOutput) > 0 {
			option.logger.Errorf("command %q error: %s",
				dvisvgmCmd, string(dvisvgmOutput))
		}
		return nil, errors.Wrap(err, "exec dvisvgm")
	}

	// Extract baseline information from the output.
	var depth float64
	depthMatch := regexpDvisvgmDepth.FindSubmatch(dvisvgmOutput)
	if len(depthMatch) >= 2 {
		depth, _ = strconv.ParseFloat(string(depthMatch[1]), 64)
	}
	baseline := 1.00375 * depth / float64(option.fontSize)

	// Execute the scour optimize command.
	scourExePath, err := exec.LookPath("scour")
	if err != nil {
		return nil, errors.Wrap(err, "lookup scour")
	}
	scourCmd := exec.CommandContext(ctx, scourExePath,
		"--shorten-ids", "--no-line-breaks", "--remove-metadata",
		"--enable-comment-stripping", "--strip-xml-prolog",
		fmt.Sprintf("--set-precision=%d", option.precision),
		fmt.Sprintf("--set-c-precision=%d", option.controlPrecision),
		"-i", "code.svg", "-o", "code.out.svg")
	scourCmd.Dir = workDir
	option.logger.Infof("command %q execute", scourCmd)
	if output, err := scourCmd.CombinedOutput(); err != nil {
		if len(output) > 0 {
			option.logger.Errorf("command %q error: %s",
				scourCmd, string(output))
		}
		return nil, errors.Wrap(err, "exec scour")
	}

	// Read the generated file back in to the DOM node and
	// return. The underlying SVG will be removed then.
	svg, err := ioutil.ReadFile(filepath.Join(workDir, "code.out.svg"))
	if err != nil {
		return nil, errors.Wrap(err, "read SVG file")
	}
	return &Result{
		Data:     svg,
		Baseline: baseline,
	}, nil
}
