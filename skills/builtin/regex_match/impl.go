//go:build wasip1

package main

import (
	"encoding/json"
	"regexp"

	"github.com/polarisagi/polaris/pkg/cognition/skill/sdk"
)

type Input struct {
	Pattern string   `json:"pattern"`
	Text    string   `json:"text"`
	Flags   []string `json:"flags"`
}

type Output struct {
	Matched bool     `json:"matched"`
	Matches []string `json:"matches"`
}

func init() {
	sdk.Register(func(input []byte) ([]byte, error) {
		var in Input
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}

		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return nil, err
		}

		matches := re.FindAllString(in.Text, -1)
		matched := len(matches) > 0

		out := Output{
			Matched: matched,
			Matches: matches,
		}

		return json.Marshal(out)
	})
}

func main() {}
