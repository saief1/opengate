# Contributing to OpenGate

Thanks for helping! OpenGate stays small and auditable on purpose, so please read this first.

## Ground rules

* **Dependencies are a security decision.** New modules need a clear justification in the PR: what they do,
  why the standard library isn't enough, and the module's own transitive dependencies. No CGO.
* **Secure by default.** New features ship off or locked down, and secrets never go into YAML or logs.
* **OpenAI compatibility.** Client-visible behaviour (requests, responses, errors, SSE) must match the OpenAI API.
* **Dashboard:** vanilla JS only, no build step, no `innerHTML` with data, and nothing that requires loosening the CSP.

## Development

```bash
make test        # all tests; no network or API keys needed (mock provider)
make vet         # go vet + gofmt
make demo        # run locally with seeded traffic
```

* Add unit tests for logic (budgets, pricing, translation) and integration tests in `integration/` for
  end-to-end behaviour, using `internal/mockprovider`.
* New provider adapters live in `internal/provider/` and must cover non-streaming, streaming, usage extraction
  and error mapping, with tests against recorded or mocked wire formats.
* Keep commits focused and write descriptive messages. By contributing you agree that your contributions are
  licensed under Apache-2.0 (see LICENSE). Add a `Signed-off-by` line (DCO) to your commits.

## Reporting bugs

Open an issue with your OpenGate version, a minimal config (no secrets), the request and the observed response.
For security issues see [SECURITY.md](SECURITY.md).
