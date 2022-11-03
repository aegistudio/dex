package main

import (
	"context"
	"os"
	"os/signal"
	"strings"

	"github.com/aegistudio/shaft"
	"github.com/aegistudio/shaft/serpent"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/aegistudio/dex/tex2svg"
)

var rootCmd = &cobra.Command{
	Use:   "dex",
	Short: "DOM TeX Transformer",
	Long: strings.Trim(`
DeX (DOM TeX Transformer) is a command line tool used for
transforming DOM nodes "<latex>", "<latexblk>", "<latexdoc>"
and dollar quoted contents ("$" and "$$") inside HTMLs and
so on, into the actual TeX-transformed result.

There're already TeX display supports like MathJax, but its
DOM parser works poorly, its transformation capability is
limited, and does not support TiKZ. So I add a TeX transform
pass instead in order to fully utilize the TeX's capability.

The command line tool requires TeX, LaTex and TexLive to be
installed. And we've provided a Docker container in which the
configurations are done and ready to go.
`, "\r\n"),
	PreRunE: serpent.Executor(shaft.Module(
		shaft.Provide(parseTeX2SVGOption),
		shaft.Provide(newLogger),
	)).PreRunE,
}

var (
	precision, controlPrecision, fontSize int
	fallable                              bool
	template, templateDir                 string

	logLevel string
)

func parseTeX2SVGOption() []tex2svg.Option {
	var result []tex2svg.Option
	if precision != 0 {
		result = append(result, tex2svg.WithPrecision(precision))
	}
	if controlPrecision != 0 {
		result = append(result,
			tex2svg.WithControlPrecision(controlPrecision))
	}
	if fontSize != 0 {
		result = append(result, tex2svg.WithFontSize(fontSize))
	}
	if template != "" {
		result = append(result, tex2svg.WithTemplate(template))
	}
	if templateDir != "" {
		result = append(result, tex2svg.WithTemplateDir(templateDir))
	}
	return result
}

func newLogger() ([]tex2svg.Option, *log.Logger, error) {
	logger := log.StandardLogger()
	if logLevel != "" {
		level, err := log.ParseLevel(logLevel)
		if err != nil {
			return nil, nil, err
		}
		logger.SetLevel(level)
	} else {
		logger.SetLevel(log.WarnLevel)
	}
	return []tex2svg.Option{tex2svg.WithLogger(logger)}, logger, nil
}

func init() {
	rootCmd.PersistentFlags().IntVar(
		&precision, "precision", precision,
		"preserved node precision of the generated SVG")
	rootCmd.PersistentFlags().IntVar(
		&controlPrecision, "control-precision", controlPrecision,
		"preserved control precision of the generated SVG")
	rootCmd.PersistentFlags().IntVar(
		&fontSize, "font-size", fontSize,
		"font size of the generated SVG")
	rootCmd.PersistentFlags().BoolVar(
		&fallable, "fallable", fallable,
		"allow fallback to latex template for unknown type")
	rootCmd.PersistentFlags().StringVar(
		&template, "template", template,
		"forcefully used template for all generation")
	rootCmd.PersistentFlags().StringVar(
		&templateDir, "template-dir", templateDir,
		"template file search path")
	rootCmd.PersistentFlags().StringVar(
		&logLevel, "log-level", logLevel,
		"log level for application logger")
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := serpent.ExecuteContext(ctx, rootCmd); err != nil {
		os.Exit(1)
	}
}
