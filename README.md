# DeX: DOM TeX Transformer

DeX is a command line for transforming TeX contents in DOMs into
their rendered form. The transformation is static and platform /
browser independent, so it eliminates the need for accompanying
complex Javascript while yielding a fully TeX-featured result.

DeX scans DOMs, matches and replaces element nodes like `<latex>`,
`<latexblk>`, `<latexdoc>` and `<tikzpicture>` and replaces them
with their generated results.
