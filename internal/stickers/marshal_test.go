package stickers

import "go.yaml.in/yaml/v3"

func marshalForTest(t *Template) ([]byte, error) { return yaml.Marshal(t) }
