// Package tex2svg provides the support for coverting the
// specified TeX input into a parsed SVG's HTML node.
//
// To work, we will first generate a TeX file and convert
// it into a PDF or DVI file using "pdflatex". Then we will
// convert the PDF file into the SVG file using "pdf2svg".
package tex2svg

import (
	"bytes"
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
	protobuf "google.golang.org/protobuf/proto"

	"github.com/aegistudio/dex/tex2svg/proto"
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

	cache Cache
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
			if errors.Is(err, os.ErrNotExist) && option.fallable {
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

// Cache is the content cache that the Generate
// function could refer to before pulling up any
// subprocess and generate content.
//
// The key and value must be transparent so that
// this package could arrange its content freely.
type Cache interface {
	Load(
		key []byte, generate func() ([]byte, error),
	) ([]byte, error)
}

// nullCache is the empty implementation of the cache.
type nullCache struct {
}

func (nullCache) Load(
	key []byte, generate func() ([]byte, error),
) ([]byte, error) {
	return generate()
}

// WithCache sets the cache function for the generator.
func WithCache(cache Cache) Option {
	return func(option *option) {
		option.cache = cache
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
	option.cache = &nullCache{}
	WithOptions(opts...)(&option)

	// Generate the latex content first.
	var buffer bytes.Buffer
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
	if err := template.Execute(&buffer, &data); err != nil {
		return nil, errors.Wrap(err, "generate TeX")
	}

	// Print out the generated content if required.
	if option.logger.IsLevelEnabled(log.DebugLevel) {
		option.logger.Debugf("tex content: %s",
			string(buffer.Bytes()))
	}

	// Prepare the key for final lookup.
	//
	// Currently we require the file from different version
	// to be incompatible with each other. The worst result
	// is to regenerate all content from scratch.
	var key proto.Key
	key.Version = proto.Version
	key.Tex = buffer.Bytes()
	key.Precision = int64(option.precision)
	key.ControlPrecision = int64(option.controlPrecision)
	keyData, err := protobuf.Marshal(&key)
	if err != nil {
		return nil, errors.Wrap(err, "marshal key")
	}
	valueData, err := option.cache.Load(keyData, func() ([]byte, error) {
		// Create the temporary directory for generated file.
		workDir, err := os.MkdirTemp("", "")
		if err != nil {
			return nil, errors.Wrap(err, "create temp dir")
		}
		defer func() { _ = os.RemoveAll(workDir) }()
		option.logger.Infof("workdir %q created", workDir)

		// Generate the TeX file as the ingredient.
		texPath := filepath.Join(workDir, "code.tex")
		if err := ioutil.WriteFile(
			texPath, buffer.Bytes(), os.FileMode(0644)); err != nil {
			return nil, errors.Wrap(err, "create TeX file")
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

		// Marshal into the value for storage.
		var value proto.Value
		value.Data = svg
		value.Baseline = baseline
		data, err := protobuf.Marshal(&value)
		if err != nil {
			return nil, errors.Wrap(err, "marshal value")
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}

	// Unmarshal the data and collect the final result.
	var value proto.Value
	if err := protobuf.Unmarshal(valueData, &value); err != nil {
		return nil, errors.Wrap(err, "unmarshal value")
	}
	return &Result{
		Data:     value.Data,
		Baseline: value.Baseline,
	}, nil
}
