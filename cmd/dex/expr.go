package main

import (
	"encoding/base64"
	"io/ioutil"
	"os"
	"strings"

	"github.com/aegistudio/shaft"
	"github.com/aegistudio/shaft/serpent"
	"github.com/spf13/cobra"

	"github.com/aegistudio/dex/tex2svg"
)

var (
	exprType  = "latex"
	exprAttrs []string
)

var cmdExpr = &cobra.Command{
	Use:   "expr",
	Short: "generate the SVG for a single expression",
	Args:  cobra.MaximumNArgs(1),
	RunE: serpent.Executor(shaft.Invoke(func(
		ctx serpent.CommandContext, args serpent.CommandArgs,
		options []tex2svg.Option,
	) error {
		// Fetch the content for generation first.
		var content string
		if len(args) == 1 {
			content = args[0]
		} else {
			b, err := ioutil.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			content = string(b)
		}

		// Parse the attributes of the templates and store
		// it inside the html.Attribute node.
		attrs := make(map[string]string)
		for _, attr := range exprAttrs {
			if index := strings.Index(attr, "="); index >= 0 {
				attrs[attr[0:index]] = attr[index+1:]
			}
		}

		// Generate SVG image for the input.
		svg, err := tex2svg.Generate(ctx, exprType, content,
			tex2svg.WithOptions(options...),
			tex2svg.WithAttributes(attrs))
		if err != nil {
			return err
		}
		println("data:image/svg+xml;base64," +
			base64.StdEncoding.EncodeToString(svg))
		return nil
	})).RunE,
}

func init() {
	cmdExpr.PersistentFlags().StringVarP(
		&exprType, "type", "t", exprType,
		"specify the template used for generation")
	cmdExpr.PersistentFlags().StringArrayVarP(
		&exprAttrs, "attr", "x", exprAttrs,
		"specify attributes for the template")
	rootCmd.AddCommand(cmdExpr)
}
