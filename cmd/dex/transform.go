package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io/fs"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/aegistudio/shaft"
	"github.com/aegistudio/shaft/serpent"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/sync/errgroup"

	"github.com/aegistudio/dex/tex2svg"
)

// checkScanNode determines whether the node is to be transformed.
//
// Only those from scanNodes will be transformed, and the other
// nodes will be ignored completely.
//
// Given a node <type id="a" class="b c">, the node will be
// scanned for transformation if any of the "type", "type#a",
// "type.b", "type.c", "#a", ".b" and ".c", is inside the
// scan node's set.
//
// XXX: the matching scheme might be changing in the future,
// depending on whether there's more refined requirements.
func checkScanNode(node *html.Node, set map[string]struct{}) bool {
	tag := node.Data
	if _, ok := set[tag]; ok {
		return true
	}
	var id, classes string
	for _, attr := range node.Attr {
		switch attr.Key {
		case "id":
			id = attr.Val
		case "class":
			classes = attr.Val
		}
	}
	if id != "" {
		if _, ok := set["#"+id]; ok {
			return true
		}
		if _, ok := set[tag+"#"+id]; ok {
			return true
		}
	}
	if classes != "" {
		for _, class := range strings.Split(classes, "") {
			if _, ok := set["."+class]; ok {
				return true
			}
			if _, ok := set[tag+"."+class]; ok {
				return true
			}
		}
	}
	return false
}

// regexpDollar is the matcher for "$" and "$$" elements.
var regexpDollar = regexp.MustCompile(`(\$\$([^$]*)\$\$|\$([^$]*)\$)`)

// replaceDollarExpr attemps to replace all '$' symbols into
// '<latex>' and '$$' symbols into '<latexblk>', so that they
// can be used for next transformation.
func replaceDollarExpr(node *html.Node, set map[string]struct{}) {
	// Check whether the current node is in set.
	if node.Type != html.ElementNode {
		return
	}
	if node.FirstChild == nil {
		return
	}
	shouldScan := checkScanNode(node, set)

	// Scan for all nodes and transform them into the nodes.
	for n := node.FirstChild; n != nil; n = n.NextSibling {
		switch n.Type {
		case html.ElementNode:
			replaceDollarExpr(n, set)
		case html.TextNode:
			if !shouldScan {
				continue
			}
			for {
				data := n.Data
				match := regexpDollar.FindStringSubmatchIndex(data)
				if len(match) == 0 {
					break
				}
				node.InsertBefore(&html.Node{
					Type: html.TextNode,
					Data: data[:match[0]],
				}, n)
				n.Data = data[match[1]:]
				var nodeTyp, nodeValue string
				if match[4] >= 0 {
					nodeTyp = "latexblk"
					nodeValue = data[match[4]:match[5]]
				} else {
					nodeTyp = "latex"
					nodeValue = data[match[6]:match[7]]
				}
				newNode := &html.Node{
					Type: html.ElementNode,
					Data: nodeTyp,
				}
				newNode.AppendChild(&html.Node{
					Type: html.TextNode,
					Data: nodeValue,
				})
				node.InsertBefore(newNode, n)
			}
		}
	}
}

// transformNode transforms a single matched node.
func transformNode(
	ctx context.Context, n *html.Node, options []tex2svg.Option,
) (*html.Node, error) {
	// Extract the content for conversion. The nodes under
	// it will be either copied or rendered.
	var b bytes.Buffer
	for m := n.FirstChild; m != nil; m = m.NextSibling {
		if err := html.Render(&b, m); err != nil {
			return nil, err
		}
	}

	// Extract information that should goes to the HTML
	// node later when it is returned, while others goes
	// to the transformer's attribtues.
	falling := make(map[string]string)
	attrs := make(map[string]string)
	for _, attr := range n.Attr {
		switch attr.Key {
		case "id", "class", "style", "alt":
			falling[attr.Key] = attr.Val
		default:
			attrs[attr.Key] = attr.Val
		}
	}

	// Perform transformation into the SVG document.
	svg, err := tex2svg.Generate(
		ctx, n.Data, b.String(),
		tex2svg.WithOptions(options...),
		tex2svg.WithAttributes(attrs),
	)
	if err != nil {
		return nil, err
	}

	// XXX: modify the class of generated node to be
	// related to our framework, so that its style could
	// be tweaked conveniently.
	newClass := "dex-" + n.Data
	if class, ok := falling["class"]; ok {
		newClass = class + " " + newClass
	}
	falling["class"] = newClass

	// Collect the result and construct the result node.
	result := &html.Node{
		Type:     html.ElementNode,
		Data:     "img",
		DataAtom: atom.Img,
	}
	result.Attr = append(result.Attr, html.Attribute{
		Key: "src",
		Val: "data:image/svg+xml;base64," +
			base64.StdEncoding.EncodeToString(svg),
	})
	for key, val := range falling {
		result.Attr = append(result.Attr, html.Attribute{
			Key: key,
			Val: val,
		})
	}
	return result, nil
}

// transformDOM attempts to scan and transform the HTML body.
//
// The node must be from the body and only those in set will be
// scanned and dispatched for transformation.
//
// The currently scanned node is asserted not to be inside the
// transform set. We usually start from the "<body>" node.
func transformDOM(
	ctx context.Context, node *html.Node, haltOnError bool,
	set map[string]struct{}, options []tex2svg.Option,
) error {
	if node.Type != html.ElementNode {
		return nil
	}
	if node.FirstChild == nil {
		return nil
	}
	n := node.FirstChild
	for n != nil {
		next := n.NextSibling
		if err := func() error {
			if n.Type != html.ElementNode {
				return nil
			}
			if _, ok := set[n.Data]; !ok {
				return transformDOM(ctx, n, haltOnError, set, options)
			}
			svgNode, err := transformNode(ctx, n, options)
			if err != nil {
				if !haltOnError {
					err = nil
				}
				return err
			}
			node.InsertBefore(svgNode, n)
			node.RemoveChild(n)
			return nil
		}(); err != nil {
			return err
		}
		n = next
	}
	return nil
}

// transformHTMLFile performs the transformation of an HTML file.
func transformHTMLFile(
	ctx context.Context, src, dst string,
	scanSet, transformSet map[string]struct{},
	haltOnError bool, options []tex2svg.Option,
) error {
	// Attempt to read and parse the HTML first.
	data, err := ioutil.ReadFile(src)
	if err != nil {
		return errors.Wrapf(err, "read file %q", src)
	}
	doc, err := html.Parse(bytes.NewBuffer(data))
	if err != nil {
		return errors.Wrapf(err, "parse file %q", src)
	}

	// Search for the HTML node. There should be only
	// a single html node in the document.
	var htmlNode *html.Node
	for n := doc.FirstChild; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode && n.DataAtom == atom.Html {
			htmlNode = n
			break
		}
	}
	if htmlNode == nil {
		return nil
	}

	// Scan for body node and start transforming.
	var bodyNode *html.Node
	for n := htmlNode.FirstChild; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode && n.DataAtom == atom.Body {
			bodyNode = n
			break
		}
	}
	if bodyNode == nil {
		return nil
	}

	// Scan and replace the double dollar expression.
	replaceDollarExpr(bodyNode, scanSet)

	// Perform recursive replacement of transform nodes.
	if err := transformDOM(
		ctx, bodyNode, haltOnError, transformSet, options); err != nil {
		return errors.Wrapf(err, "transform file %q", src)
	}

	// Render the translated DOM and write to the file.
	var b bytes.Buffer
	if err := html.Render(&b, doc); err != nil {
		return errors.Wrapf(err, "render file %q", src)
	}
	return ioutil.WriteFile(dst, b.Bytes(), fs.FileMode(0644))
}

// sniffLen is the length for http.DetectContentType required
// for detecting the underlying MIME type.
const sniffLen = 512

// isHTMLFile checks whether the file is HTML and emits true
// if the file is going to be checked for generation.
func isHTMLFile(path string, info fs.FileInfo) (bool, error) {
	if !info.Mode().IsRegular() {
		return false, nil
	}

	// Attempt to open the file and read some content from file.
	f, err := os.Open(path)
	if err != nil {
		return false, errors.Wrapf(err, "open file %q", path)
	}
	defer func() { _ = f.Close() }()
	var buf [sniffLen]byte
	length, err := f.Read(buf[:])
	if err != nil {
		return false, errors.Wrapf(err, "read file %q", path)
	}
	b := buf[:length]

	// Pass the content to sniffing and return the result.
	return strings.HasPrefix(http.DetectContentType(b), "text/html"), nil
}

type transformTask struct {
	src, dst string
}

// runDispatchThread executes the dispatching thread.
//
// Dispatching thread scans for files, judging their file
// types and send tasks to transformer threads.
func runDispatchThread(
	ctx context.Context, inputDir, outputDir string,
	taskCh chan<- transformTask,
) error {
	return filepath.Walk(inputDir, func(
		srcPath string, info fs.FileInfo, err error,
	) error {
		if err != nil {
			return err
		}
		mode := info.Mode()
		if !mode.IsDir() && !mode.IsRegular() {
			return nil
		}

		// Make directory in specified output directory.
		relPath, err := filepath.Rel(inputDir, srcPath)
		if err != nil {
			return errors.Wrapf(err, "eval relpath %q", srcPath)
		}
		dstPath := filepath.Join(outputDir, relPath)
		if info.IsDir() {
			if err := os.MkdirAll(dstPath, fs.FileMode(0755)); err != nil {
				return errors.Wrapf(err, "mkdir output %q", srcPath)
			}
			return nil
		}

		// Check whether the specified file is HTML.
		ok, err := isHTMLFile(srcPath, info)
		if err != nil {
			return errors.Wrapf(err, "check input %q", srcPath)
		}
		if !ok {
			return nil
		}

		// Dispatch tasks to the transformer's task queue.
		task := transformTask{
			src: srcPath,
			dst: dstPath,
		}
		select {
		case <-ctx.Done():
			if mode.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case taskCh <- task:
		}
		return nil
	})
}

// runWorkerThread consumes the task channel and work.
func runWorkerThread(
	ctx context.Context, taskCh <-chan transformTask,
	scanSet, transformSet map[string]struct{},
	haltOnError bool, options []tex2svg.Option,
) error {
	for {
		var task transformTask
		var ok bool
		select {
		case <-ctx.Done():
			return nil
		case task, ok = <-taskCh:
			if !ok {
				return nil
			}
			if err := transformHTMLFile(
				ctx, task.src, task.dst,
				scanSet, transformSet, haltOnError, options); err != nil {
				return err
			}
		}
	}
}

var (
	transformHaltOnError = true
	transformJobs        int
	transformInputDir    string
	transformOutputDir   string

	transformScanNode = []string{
		"a", "b", "strong", "i", "p", "li",
		"h1", "h2", "h3", "h4", "h5",
	}
	transformTransformNode = []string{
		"latex", "latexblk", "latexdoc", "tikz",
	}
)

var cmdTransform = &cobra.Command{
	Use:   "transform",
	Short: "scan HTML documents and perform transmation",
	RunE: serpent.Executor(shaft.Invoke(func(
		commandCtx serpent.CommandContext, args serpent.CommandArgs,
		options []tex2svg.Option,
	) error {
		// Evaluate actual path for dispatching tasks.
		inputDir := transformInputDir
		if inputDir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return errors.Wrap(err, "get workdir")
			}
			inputDir = wd
		}
		inputStat, err := os.Stat(inputDir)
		if err != nil {
			return errors.Wrapf(err, "stat input dir %q", inputDir)
		}
		if !inputStat.Mode().IsDir() {
			return errors.Errorf("input dir %q not directory", inputDir)
		}
		outputDir := transformOutputDir
		if outputDir == "" {
			outputDir = inputDir
		}
		outputStat, err := os.Stat(outputDir)
		if err != nil {
			return errors.Wrapf(err, "stat output dir %q", outputDir)
		}
		if !outputStat.Mode().IsDir() {
			return errors.Errorf("output dir %q not directory", outputDir)
		}

		// Evaluate the scan and transform set.
		scanSet := make(map[string]struct{})
		for _, v := range transformScanNode {
			scanSet[v] = struct{}{}
		}
		transformSet := make(map[string]struct{})
		for _, v := range transformTransformNode {
			transformSet[v] = struct{}{}
		}

		// Create the error group for starting tasks.
		group, ctx := errgroup.WithContext(commandCtx)

		// Create the dispatcher thread and dispatcher channel.
		taskCh := make(chan transformTask)
		group.Go(func() error {
			defer close(taskCh)
			return runDispatchThread(ctx, inputDir, outputDir, taskCh)
		})

		// Start worker threads for performing transformation.
		jobs := transformJobs
		if jobs == 0 {
			jobs = runtime.GOMAXPROCS(0)
		}
		if jobs == 0 {
			jobs = 1
		}
		for i := 0; i < jobs; i++ {
			group.Go(func() error {
				return runWorkerThread(
					ctx, taskCh, scanSet, transformSet,
					transformHaltOnError, options)
			})
		}

		// Wait for the transformation to be done.
		return group.Wait()
	})).RunE,
}

func init() {
	cmdTransform.PersistentFlags().BoolVar(
		&transformHaltOnError, "halt-on-error", transformHaltOnError,
		"stop transformation when error is encountered")
	cmdTransform.PersistentFlags().IntVarP(
		&transformJobs, "jobs", "j", transformJobs,
		"number of parallel jobs")
	cmdTransform.PersistentFlags().StringVarP(
		&transformInputDir, "input-dir", "i", transformInputDir,
		"directory to scan for document files")
	cmdTransform.PersistentFlags().StringVarP(
		&transformOutputDir, "output-dir", "o", transformOutputDir,
		"directory to write transformed document files")
	cmdTransform.PersistentFlags().StringSliceVarP(
		&transformScanNode, "scan", "s", transformScanNode,
		"tags to be scanned for dollar expressions")
	cmdTransform.PersistentFlags().StringSliceVarP(
		&transformTransformNode, "transform", "t", transformTransformNode,
		"tags to be transformed into rendered image")
	rootCmd.AddCommand(cmdTransform)
}
