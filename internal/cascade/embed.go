package cascade

import _ "embed"

//go:embed cascade-rules.json
var cascadeRulesJSON []byte

//go:embed table-search-schema.json
var tableSearchSchemaJSON []byte
