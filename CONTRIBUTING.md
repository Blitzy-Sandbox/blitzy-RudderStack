# Contributing to RudderStack

Thanks for taking the time and for your help in improving this project!

## Table of contents

- [RudderStack Contributor Agreement](#rudderstack-contributor-agreement)
- [How You Can Contribute to RudderStack](#how-you-can-contribute-to-rudderstack)
- [Submitting a Pull Request](#submitting-a-pull-request)
- [Committing](#committing)
- [Installing and Setting Up RudderStack](#installing-and-setting-up-rudderstack)
- [Contributing Documentation](#contributing-documentation)
- [Getting Help](#getting-help)

## RudderStack Contributor Agreement

To contribute to this project, we need you to sign the [**Contributor License Agreement (“CLA”)**][CLA] for the first commit you make. By agreeing to the [**CLA**][CLA]
we can add you to list of approved contributors and review the changes proposed by you.

## How you can contribute to RudderStack

You can contribute to any open-source RudderStack project. View our [**GitHub page**](https://github.com/rudderlabs) to see all the different projects. If you encounter a bug or have an improvement suggestion, you can [**submit an issue**](https://github.com/rudderlabs/rudder-server/issues/new) describing your proposed change.

One way you can contribute to RudderStack is to create an integration. An integration is a connection between RudderStack and a downstream destination where you would like to send your event data. There are several reasons why you may want to build an integration:

- If you would like to send data to a certain destination, but RudderStack doesn't support it yet.
- If you have developed a tool that you would like RudderStack to integrate with, to expand your user base.
- If you want to add features to an already existing integration, and many more!

For more information on the different ways in which you can contribute to RudderStack, you can chat with us on our [**Slack**](https://rudderstack.com/join-rudderstack-slack-community/) channel.

> **Note:**  For creating an integration, the primary GitHub repository you will need to work with will be [**`rudder-transformer`**](https://github.com/rudderlabs/rudder-transformer).

## Submitting a pull request

The type of change you make will dictate what repositories you will need to make pull requests for. You can reach out to us on our [**Slack**](https://rudderstack.com/join-rudderstack-slack-community/) channel if you have any questions.

For instance, to create a PR for contributing to a new third-party integration, follow these instructions on [**submitting an integration PR**](https://docs.rudderstack.com/user-guides/how-to-guides/how-to-submit-an-integration-pull-request).

## Committing

We prefer squash or rebase commits so that all changes from a branch are committed to master as a single commit. All pull requests are squashed when merged, but rebasing prior to merge gives you better control over the commit message.

## Installing and setting up RudderStack

To contribute to this project, you may need to install RudderStack on your machine. You can do so by following our [**docs**](https://docs.rudderstack.com/get-started/installing-and-setting-up-rudderstack) and set up RudderStack in no time.

## Contributing documentation

All project documentation lives in the [`docs/`](docs/) directory and is authored in Markdown (`.md`) format. Architectural and workflow diagrams use [Mermaid](https://mermaid-js.github.io/) syntax embedded directly in Markdown files. Contributions that improve, expand, or correct the documentation are welcome alongside code contributions.

### Documentation style guide

Documentation should follow the established patterns used in the repository:

- **Comprehensive architecture with component breakdowns** — as demonstrated in [`services/oauth/README.md`](services/oauth/README.md).
- **Developer onboarding with architecture diagrams, interfaces, and examples** — as demonstrated in [`router/batchrouter/asyncdestinationmanager/README.md`](router/batchrouter/asyncdestinationmanager/README.md).

### Documentation types

The documentation set covers the following categories:

- **Architecture docs** — System design, data flows, component relationships, and deployment topologies.
- **API references** — Endpoint specifications, request/response schemas, authentication, and error codes.
- **Integration guides** — Source SDK setup, destination connector configuration, and warehouse connector guides.
- **Operational guides** — Warehouse sync, event replay, capacity planning, and privacy compliance.
- **Gap analysis reports** — Feature-by-feature comparison between RudderStack and Segment capabilities.

### Documentation PR requirements

When submitting a documentation pull request, ensure the following:

- Follow Markdown formatting with proper heading hierarchy (`#` for titles, `##` for sections, `###` for subsections).
- Include Mermaid diagrams for all architectural and workflow documentation (use triple-backtick `mermaid` code blocks).
- Provide source code citations for technical details in the format `Source: /path/to/file.go:LineRange`.
- Use consistent terminology from the unified glossary ([`docs/reference/glossary.md`](docs/reference/glossary.md)).
- Include code examples in Go, JavaScript, and Python with appropriate syntax highlighting.

### Documentation resources

- [Documentation Home](docs/README.md) — Documentation landing page and navigation hub.
- [Development Guide](docs/contributing/development.md) — Development environment setup and build guide.
- [Destination Onboarding](docs/contributing/destination-onboarding.md) — New destination connector onboarding guide.
- [Testing Guide](docs/contributing/testing.md) — Test infrastructure and guidelines.

## Getting help

For any questions, concerns, or queries, you can start by asking a question on our [**Slack**](https://rudderstack.com/join-rudderstack-slack-community/) channel.
<br><br>

### We look forward to your feedback on improving this project!


<!----variables---->

[issue]: https://github.com/rudderlabs/rudder-server/issues/new
[CLA]: https://forms.gle/845JRGVZaC6kPZy68
