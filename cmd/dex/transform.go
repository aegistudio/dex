package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	textTemplate "text/template"

	"github.com/aegistudio/shaft"
	"github.com/aegistudio/shaft/serpent"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/sync/errgroup"

	"github.com/aegistudio/dex/tex2cache"
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
	ctx context.Context, n *html.Node,
	options []tex2svg.Option,
) (*tex2svg.Result, map[string]string, error) {
	// Extract the content for conversion.
	var b bytes.Buffer
	for m := n.FirstChild; m != nil; m = m.NextSibling {
		// XXX: containing nodes other than text node is illformed
		// and we will just concatenate all text nodes.
		if m.Type != html.TextNode {
			return nil, nil, errors.Errorf(
				"unexpected non-text node under %q", n.Data)
		}
		if _, err := b.WriteString(m.Data); err != nil {
			return nil, nil, err
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
	generated, err := tex2svg.Generate(
		ctx, n.Data, b.String(),
		tex2svg.WithOptions(options...),
		tex2svg.WithAttributes(attrs),
	)
	if err != nil {
		return nil, nil, err
	}
	return generated, falling, nil
}

// extractDOM attempts to extract pending tasks inside HTML.
//
// The extracted nodes have their sibling and parenting
// information, so we can manipulate them even without knowing
// their position in DOM.
//
// The currently scanned node is asserted not to be inside the
// transform set. We usually start from the "<body>" node.
func extractDOM(node *html.Node, set map[string]struct{}) []*html.Node {
	if node.Type != html.ElementNode {
		return nil
	}
	if node.FirstChild == nil {
		return nil
	}
	var nodes []*html.Node
	for n := node.FirstChild; n != nil; n = n.NextSibling {
		if n.Type != html.ElementNode {
			continue
		}
		if _, ok := set[n.Data]; ok {
			nodes = append(nodes, n)
		} else {
			nodes = append(nodes, extractDOM(n, set)...)
		}
	}
	return nodes
}

// templateStyle is the template of a style item.
var templateStyle = textTemplate.Must(textTemplate.New("").Parse(`
.{{.Class}} {
	content: url({{.Image}});
	vertical-align: {{.VerticalAlign}};
	display: inline-block; width: fit-content; height: fit-content;
}
`))

// transformResultKey is the content key for identifying each
// generated result and aggregate them.
type transformResultKey struct {
	content  string
	baseline float64
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
	var headNode *html.Node
	for n := htmlNode.FirstChild; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode && n.DataAtom == atom.Head {
			headNode = n
			break
		}
	}
	if headNode == nil {
		return nil
	}
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

	// Store the transformation result so that we can
	// aggregate elements with identical image together.
	nodes := extractDOM(bodyNode, transformSet)
	results := make(map[transformResultKey]int)
	lastIndex := 0
	for _, n := range nodes {
		result, attrs, err := transformNode(ctx, n, options)
		if err != nil {
			if haltOnError {
				return errors.Wrapf(
					err, "transform file %q", src)
			}
			continue
		}
		key := transformResultKey{
			content: "data:image/svg+xml;base64," +
				base64.StdEncoding.EncodeToString(result.Data),
			baseline: result.Baseline,
		}
		id, ok := results[key]
		if !ok {
			id = lastIndex
			lastIndex++
			results[key] = id
		}

		// Create a new class for the generated node.
		newClass := fmt.Sprintf("dex-%x", id)
		if class, ok := attrs["class"]; ok {
			newClass = class + " " + newClass
		}
		attrs["class"] = newClass
		var attrKeys []string
		for attr := range attrs {
			attrKeys = append(attrKeys, attr)
		}
		sort.Strings(attrKeys)
		svgNode := &html.Node{
			Type: html.ElementNode,
			Data: "dex",
		}
		for _, attr := range attrKeys {
			svgNode.Attr = append(svgNode.Attr, html.Attribute{
				Key: attr,
				Val: attrs[attr],
			})
		}
		parent := n.Parent
		parent.InsertBefore(svgNode, n)
		parent.RemoveChild(n)
	}

	// Insert the style blocks after the HTML block.
	if len(nodes) > 0 {
		orderedResult := make([]transformResultKey, lastIndex)
		for result, i := range results {
			orderedResult[i] = result
		}
		var styleBuffer bytes.Buffer
		for id, result := range orderedResult {
			var data struct {
				Class         string
				Image         string
				VerticalAlign string
			}
			data.Class = fmt.Sprintf("dex-%x", id)
			data.Image = result.content
			data.VerticalAlign = fmt.Sprintf("%.6fem", -result.baseline)
			if err := templateStyle.Execute(
				&styleBuffer, &data); err != nil {
				return errors.Wrapf(err, "write style buffer %q", src)
			}
		}
		styleNode := &html.Node{
			Type:     html.ElementNode,
			Data:     "style",
			DataAtom: atom.Style,
		}
		styleNode.Attr = append(styleNode.Attr, html.Attribute{
			Key: "type",
			Val: "text/css",
		})
		styleNode.AppendChild(&html.Node{
			Type: html.TextNode,
			Data: styleBuffer.String(),
		})
		headNode.AppendChild(styleNode)
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

// runHTMLWorkerThread consumes the task channel and work.
func runHTMLWorkerThread(
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

	transformCacheDir    = ".cache"
	transformCacheOutput = ""
	transformCacheSize   = 1024
)

var cmdTransform = &cobra.Command{
	Use:   "transform",
	Short: "scan HTML documents and perform transmation",
	RunE: serpent.Executor(shaft.Invoke(func(
		commandCtx serpent.CommandContext, args serpent.CommandArgs,
		options []tex2svg.Option, log *logrus.Logger,
	) (rerr error) {
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

		// Evaluates the number of threads to start.
		jobs := transformJobs
		if jobs == 0 {
			jobs = runtime.GOMAXPROCS(0)
		}
		if jobs == 0 {
			jobs = 1
		}
		log.Infof("dispatching %d tasks", jobs)

		// Create the node group, which is an ambient group but
		// will wait for the tasks to complete.
		rootCtx, rootCancel := context.WithCancel(commandCtx)
		daemonGroup, daemonCtx := errgroup.WithContext(rootCtx)
		defer func() {
			if err := daemonGroup.Wait(); err != nil && rerr == nil {
				rerr = err
			}
		}()
		defer rootCancel()

		// Create the persistent cache and add it to option.
		cacheOutputDir := transformCacheDir
		if transformCacheOutput != "" {
			cacheOutputDir = transformCacheOutput
		}
		cache, err := tex2cache.New(
			daemonCtx, daemonGroup,
			transformCacheDir, cacheOutputDir,
			transformCacheSize)
		if err != nil {
			return err
		}
		options = append(options, tex2svg.WithCache(cache))

		// Create the error group for foreground tasks.
		group, ctx := errgroup.WithContext(daemonCtx)

		// Create the dispatcher thread and dispatcher channel.
		taskCh := make(chan transformTask)
		group.Go(func() error {
			defer close(taskCh)
			return runDispatchThread(ctx, inputDir, outputDir, taskCh)
		})

		// Start worker threads for performing transformation.
		for i := 0; i < jobs; i++ {
			group.Go(func() error {
				return runHTMLWorkerThread(
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
	cmdTransform.PersistentFlags().StringVar(
		&transformCacheDir, "cache-dir", transformCacheDir,
		"directory to read and write the cache")
	cmdTransform.PersistentFlags().StringVar(
		&transformCacheDir, "cache-output", transformCacheDir,
		"directory to alternative output of the cache")
	cmdTransform.PersistentFlags().IntVar(
		&transformCacheSize, "cache-size", transformCacheSize,
		"size of the cache to store in memory item")
	rootCmd.AddCommand(cmdTransform)
}
