// Package coreutils provides small, composable implementations of common
// Unix utilities.
package coreutils

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// Command is a core utility that reads and writes streams.
type Command struct {
	Name        string
	Description string
	Run         func(context.Context, []string, io.Reader, io.Writer, io.Writer) error
}

var commands = map[string]Command{}

func register(command Command) {
	if _, exists := commands[command.Name]; exists {
		panic("duplicate coreutil: " + command.Name)
	}
	commands[command.Name] = command
}

// Commands returns all available commands, sorted by name.
func Commands() []Command {
	result := make([]Command, 0, len(commands))
	for _, command := range commands {
		result = append(result, command)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Run executes a named utility.
func Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command, ok := commands[name]
	if !ok {
		return fmt.Errorf("unknown utility %q", name)
	}
	return command.Run(ctx, args, stdin, stdout, stderr)
}
