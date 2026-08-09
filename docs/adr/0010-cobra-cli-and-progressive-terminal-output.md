# ADR 0010: Use Cobra and progressive terminal output

- Status: Accepted
- Date: 2026-08-09

## Context

The first CLI grew around independent standard-library `flag.FlagSet` handlers. That kept the
binary small, but duplicated discovery metadata and made nested help, typo suggestions, shell
completion, aliases, and man-page generation inconsistent. InferCrane also needs a polished
interactive experience without allowing terminal animation to change durable operation semantics
or corrupt CI/JSON output.

## Decision

Cobra owns the public command tree, grouped help, aliases, version handling, suggestions, shell
completion, and dynamic completion hooks. Existing command handlers remain the API clients while
their option parsing is migrated incrementally behind the stable Cobra command surface. New public
commands must be declared in Cobra first and must not create another dispatcher.

Terminal presentation follows progressive enhancement. Plain, line-oriented output is the
baseline. Interactive progress may emphasize changed durable steps, but it must preserve operation
IDs, retry keys, warnings, and resume commands. JSON is never decorated. Redirected and CI output
must remain stable, and color must be optional. InferCrane does not use a full-screen TUI or an
alternate screen for lifecycle operations.

The CLI provides generated Bash, Zsh, Fish, and PowerShell completion. Completion may read the
authenticated control API for resource names, but it is best-effort, read-only, bounded by the
ordinary client timeout, and silent on failure.

## Consequences

Users get conventional infrastructure-tool navigation, offline help, typo suggestions, packaged
completion, and resource-aware completion. The project accepts Cobra, pflag, and Cobra's platform
helper as release dependencies. Command behavior remains testable without a terminal.

The CLI will not adopt Bubble Tea, Huh, or Lip Gloss for v0.1. They remain possible for a future
optional setup form or bounded renderer, but are not required to solve command structure and could
make automation or copied evidence less predictable.

## Alternatives

Continuing with only `flag.FlagSet` was rejected because the project was rebuilding established
command-tree behavior. urfave/cli and Kong were viable, but Cobra has the strongest infrastructure
tool conventions and completion ecosystem. A full-screen Bubble Tea application was rejected
because durable operations should remain scrollable, copyable, and useful after disconnect.

## Verification

CLI tests cover offline help, generated completion, unknown-command errors, inference requests,
context migration and selection, live event filtering, and authenticated identity. Release
packaging installs generated completions. Human, streaming, and JSON command paths remain covered
separately.

