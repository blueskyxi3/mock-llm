package main

import (
	"encoding/json"
	"os"

	"mock-llm/internal/server"
)

func dumpConfig() {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(server.DefaultConfig()); err != nil {
		panic(err)
	}
}
