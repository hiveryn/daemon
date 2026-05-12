package architectfs

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type markdownDocument struct {
	Metadata *yaml.Node
	Body     string
}

func readMarkdownDocument(path string) (markdownDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return markdownDocument{}, err
	}
	return parseMarkdownDocument(string(data))
}

func writeMarkdownDocument(path string, doc markdownDocument) error {
	content, err := renderMarkdownDocument(doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write markdown document %q: %w", path, err)
	}
	return nil
}

func parseMarkdownDocument(content string) (markdownDocument, error) {
	if !strings.HasPrefix(content, "---\n") {
		return markdownDocument{Body: content}, nil
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return markdownDocument{}, fmt.Errorf("parse frontmatter: missing closing delimiter")
	}
	frontmatter := content[4 : 4+end]
	body := content[4+end+5:]
	body = strings.TrimPrefix(body, "\n")
	var parsed yaml.Node
	if err := yaml.Unmarshal([]byte(frontmatter), &parsed); err != nil {
		return markdownDocument{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	meta := &parsed
	if parsed.Kind == yaml.DocumentNode && len(parsed.Content) > 0 {
		meta = parsed.Content[0]
	}
	return markdownDocument{Metadata: meta, Body: body}, nil
}

func renderMarkdownDocument(doc markdownDocument) (string, error) {
	if doc.Metadata == nil || len(doc.Metadata.Content) == 0 {
		return doc.Body, nil
	}
	data, err := yaml.Marshal(doc.Metadata)
	if err != nil {
		return "", fmt.Errorf("marshal frontmatter: %w", err)
	}
	body := doc.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return "---\n" + string(data) + "---\n\n" + body, nil
}

func newMappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func setNodeString(node *yaml.Node, key, value string) {
	setMappingValue(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func setNodeTime(node *yaml.Node, key string, value time.Time) {
	setMappingValue(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value.UTC().Format(time.RFC3339Nano)})
}

func setNodeStrings(node *yaml.Node, key string, values []string) {
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, value := range values {
		sequence.Content = append(sequence.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	setMappingValue(node, key, sequence)
}

func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	if node == nil {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

func removeMappingValue(node *yaml.Node, key string) {
	if node == nil {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		node.Content = append(node.Content[:i], node.Content[i+2:]...)
		return
	}
}
