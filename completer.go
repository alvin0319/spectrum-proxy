package main

import (
	"strings"

	"github.com/cooldogedev/spectrum"
	"github.com/elk-language/go-prompt"
	istrings "github.com/elk-language/go-prompt/strings"
)

type Completer struct {
	p *spectrum.Spectrum
}

func (c *Completer) Complete(in prompt.Document) ([]prompt.Suggest, istrings.RuneNumber, istrings.RuneNumber) {
	endIndex := in.CurrentRuneIndex()
	w := in.GetWordBeforeCursor()
	if w == "" {
		return []prompt.Suggest{}, 0, 0
	}
	startIndex := endIndex - istrings.RuneCountInString(w)

	args := strings.Split(in.TextBeforeCursor(), " ")
	if len(args) <= 1 {
		return c.completeCommand(args[0]), startIndex, endIndex
	}

	// Handle command-specific completions
	switch args[0] {
	case "transfer":
		if len(args) == 2 {
			return c.completePlayerNames(args[1]), startIndex, endIndex
		} else if len(args) == 3 {
			return c.completeServerNames(args[2]), startIndex, endIndex
		}
	case "players":
		return []prompt.Suggest{}, 0, 0
	case "info":
		return []prompt.Suggest{}, 0, 0
	}

	return []prompt.Suggest{}, 0, 0
}

// completeCommand provides suggestions for the main commands
func (c *Completer) completeCommand(input string) []prompt.Suggest {
	commands := []prompt.Suggest{
		{Text: "players", Description: "List all connected players"},
		{Text: "transfer", Description: "Transfer a player to another server"},
		{Text: "info", Description: "Show server information"},
		{Text: "stop", Description: "Stop the server"},
		{Text: "exit", Description: "Stop the server"},
	}

	return prompt.FilterHasPrefix(commands, input, true)
}

// completePlayerNames provides suggestions for player names
func (c *Completer) completePlayerNames(input string) []prompt.Suggest {
	var suggestions []prompt.Suggest

	for _, session := range c.p.Registry().GetSessions() {
		playerName := session.Client().IdentityData().DisplayName
		suggestions = append(suggestions, prompt.Suggest{
			Text:        playerName,
			Description: "Connected player",
		})
	}

	return prompt.FilterHasPrefix(suggestions, input, true)
}

// completeServerNames provides suggestions for server names
func (c *Completer) completeServerNames(input string) []prompt.Suggest {
	var suggestions []prompt.Suggest

	for serverName, _ := range serverMap {
		suggestions = append(suggestions, prompt.Suggest{
			Text:        serverName,
			Description: "Available server",
		})
	}

	return prompt.FilterHasPrefix(suggestions, input, true)
}

// NewCompleter creates a new Completer instance
func NewCompleter(p *spectrum.Spectrum) *Completer {
	return &Completer{p: p}
}
