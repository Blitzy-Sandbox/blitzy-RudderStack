<p align="center">
📖 Just launched <b><a href="https://www.rudderstack.com/learn/">Data Learning Center</a></b> - Resources on data engineering and data infrastructure
  <br/>
 </p>

<p align="center">
  <a href="https://www.rudderstack.com/">
    <img src="resources/rs-logo-full-duotone-dark.jpg" height="64px">
  </a>
</p>

<p align="center"><b>The Customer Data Platform for Developers</b></p>

<p align="center">
  <a href="https://github.com/rudderlabs/rudder-server/actions/workflows/tests.yaml">
    <img src="https://github.com/rudderlabs/rudder-server/actions/workflows/tests.yaml/badge.svg">
  </a>
  <a href="https://github.com/rudderlabs/rudder-server/actions/workflows/builds.yml">
    <img src="https://github.com/rudderlabs/rudder-server/actions/workflows/builds.yml/badge.svg">
  </a>
  <a href="https://goreportcard.com/report/github.com/rudderlabs/rudder-server">
    <img src="https://goreportcard.com/badge/github.com/rudderlabs/rudder-server">
  </a>
  <a href="https://github.com/rudderlabs/rudder-server/releases">
    <img src="https://img.shields.io/github/v/release/rudderlabs/rudder-server?color=blue&sort=semver">
  </a>
  <a href="https://www.rudderstack.com/docs/get-started/installing-and-setting-up-rudderstack/docker/">
    <img src="https://img.shields.io/docker/pulls/rudderlabs/rudder-server">
  </a>
  <a href="https://github.com/rudderlabs/rudder-server/blob/master/LICENSE">
    <img src="https://img.shields.io/static/v1?label=license&message=ELv2&color=7447fc">
  </a>
</p>

<p align="center">
  <b>
    <a href="https://www.rudderstack.com/">Website</a>
    ·
    <a href="https://www.rudderstack.com/docs/">Documentation</a>
    ·
    <a href="docs/README.md">Docs</a>
    ·
    <a href="https://github.com/rudderlabs/rudder-server/blob/master/CHANGELOG.md">Changelog</a>
    ·
    <a href="https://www.rudderstack.com/blog/">Blog</a>
    ·
    <a href="https://www.rudderstack.com/join-rudderstack-slack-community/">Slack</a>
    ·
    <a href="https://twitter.com/rudderstack">Twitter</a>
  </b>
</p>

---

As the leading open source Customer Data Platform (CDP), [**RudderStack**](https://www.rudderstack.com/) provides data pipelines that make it easy to collect data from every application, website and SaaS platform, then activate it in your warehouse and business tools.

With RudderStack, you can build customer data pipelines that connect your whole customer data stack and then make them smarter by triggering enrichment and activation in customer tools based on analysis in your data warehouse. It's easy-to-use SDKs and event source integrations, Cloud Extract integrations, transformations, and expansive library of destination and warehouse integrations makes building customer data pipelines for both event streaming and cloud-to-warehouse ELT simple.

<p align="center">
  <a href="https://www.rudderstack.com/">
    <img src="https://user-images.githubusercontent.com/59817155/121468374-4ef91e00-c9d8-11eb-8611-28bea18f609d.gif" alt="RudderStack">
  </a>
</p>

| Try **RudderStack Cloud Free** - a free tier of [**RudderStack Cloud**](https://www.rudderstack.com/cloud/). Click [**here**](https://app.rudderstack.com/signup?type=freetrial) to start building a smarter customer data pipeline today, with RudderStack Cloud. |
| :----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |

## Key features

- **Warehouse-first**: RudderStack treats your data warehouse as a first class citizen among destinations, with advanced features and configurable, near real-time sync.

- **Developer-focused**: RudderStack is built API-first. It integrates seamlessly with the tools that the developers already use and love.

- **High Availability**: RudderStack comes with at least 99.99% uptime. We have built a sophisticated error handling and retry system that ensures that your data will be delivered even in the event of network partitions or destinations downtime.

- **Privacy and Security**: You can collect and store your customer data without sending everything to a third-party vendor. With RudderStack, you get fine-grained control over what data to forward to which analytical tool.

- **Unlimited Events**: Event volume-based pricing of most of the commercial systems is broken. With RudderStack Open Source, you can collect as much data as possible without worrying about overrunning your event budgets.

- **Segment API-compatible**: RudderStack is fully compatible with the Segment API and achieves **100% field-level parity** with the [Twilio Segment Event Specification](https://segment.com/docs/connections/spec/) across all six core event types (`identify`, `track`, `page`, `screen`, `group`, `alias`), including structured Client Hints pass-through (`context.userAgentData`) and semantic event category support. So you don't need to change your app if you are using Segment; just integrate the RudderStack SDKs into your app and your events will keep flowing to the destinations (including data warehouses) as before.

- **Production-ready**: Companies like Mattermost, IFTTT, Torpedo, Grofers, 1mg, Nana, OnceHub, and dozens of large companies use RudderStack for collecting their events.

- **Seamless Integration**: RudderStack currently supports integration with over 90 popular [**tool**](https://www.rudderstack.com/docs/destinations/) and [**warehouse**](https://www.rudderstack.com/docs/data-warehouse-integrations/) destinations.

- **User-specified Transformation**: RudderStack offers a powerful JavaScript-based event transformation framework which lets you enhance or transform your event data by combining it with your other internal data. Furthermore, as RudderStack runs inside your cloud or on-premise environment, you can easily access your production data to join with the event data.

## Get started

The easiest way to experience RudderStack is to [**sign up**](https://app.rudderstack.com/signup?type=freetrial) for **RudderStack Cloud Free** - a completely free tier of [**RudderStack Cloud**](https://www.rudderstack.com/cloud/).

You can also set up RudderStack on your platform of choice with these two easy steps:

### Step 1: Set up RudderStack

- [**Docker**](https://www.rudderstack.com/docs/rudderstack-open-source/installing-and-setting-up-rudderstack/docker/)
- [**Kubernetes**](https://www.rudderstack.com/docs/rudderstack-open-source/installing-and-setting-up-rudderstack/kubernetes/)
- [**Developer machine setup**](https://www.rudderstack.com/docs/rudderstack-open-source/installing-and-setting-up-rudderstack/developer-machine-setup/)

> **Note**: If you are planning to use RudderStack in production, we STRONGLY recommend using our Kubernetes Helm charts. We update our Docker images with bug fixes much more frequently than our GitHub repo.

### Step 2: Verify the installation

Once you have installed RudderStack, [**send test events**](https://www.rudderstack.com/docs/get-started/installing-and-setting-up-rudderstack/sending-test-events/) to verify the setup.

## Architecture

RudderStack is an independent, stand-alone system with a dependency only on the database (PostgreSQL). Its backend is written in **Go** with a rich UI written in **React.js**.

A high-level view of RudderStack’s architecture is shown below:

![Architecture](resources/rudder-server-architecture.png)

For more details on the various architectural components, refer to our [**documentation**](https://www.rudderstack.com/docs/get-started/rudderstack-architecture/).

For detailed architecture documentation, see the [Architecture Overview](docs/architecture/overview.md). See also: [Data Flow](docs/architecture/data-flow.md) | [Pipeline Stages](docs/architecture/pipeline-stages.md) | [Deployment Topologies](docs/architecture/deployment-topologies.md) | [Warehouse State Machine](docs/architecture/warehouse-state-machine.md)

## 📚 Documentation

Comprehensive documentation is available in the [`docs/`](docs/README.md) directory, covering architecture, API references, integration guides, operational runbooks, and Segment parity analysis.

| Category | Description |
|----------|-------------|
| **[Gap Report](docs/gap-report/index.md)** | Segment parity gap analysis and sprint roadmap |
| **[Architecture](docs/architecture/overview.md)** | System architecture, data flows, deployment topologies |
| **[API Reference](docs/api-reference/index.md)** | HTTP API, Event Spec, gRPC API, error codes |
| **[Getting Started](docs/guides/getting-started/installation.md)** | Installation, configuration, first events |
| **[Migration Guide](docs/guides/migration/segment-migration.md)** | Segment-to-RudderStack migration |
| **[Source SDKs](docs/guides/sources/javascript-sdk.md)** | JavaScript, iOS, Android, server-side SDK guides |
| **[Destinations](docs/guides/destinations/index.md)** | Stream, cloud, and warehouse destination guides |
| **[Transformations](docs/guides/transformations/overview.md)** | Custom transforms and Functions |
| **[Governance](docs/guides/governance/tracking-plans.md)** | Tracking plans, consent, event filtering |
| **[Identity](docs/guides/identity/identity-resolution.md)** | Identity resolution and profiles |
| **[Operations](docs/guides/operations/warehouse-sync.md)** | Warehouse sync, replay, capacity planning |
| **[Warehouse Connectors](docs/warehouse/overview.md)** | Per-warehouse setup and configuration guides |
| **[Reference](docs/reference/config-reference.md)** | Configuration, environment variables, glossary |
| **[Contributing](docs/contributing/development.md)** | Development setup, destination onboarding, testing |

### Segment Parity Gap Report

A comprehensive gap analysis comparing RudderStack capabilities against Twilio Segment features is available in the [Gap Report](docs/gap-report/index.md). The **Event Spec Parity** dimension has achieved **100% field-level parity** with the Twilio Segment Event Specification, covering all six core event types (`identify`, `track`, `page`, `screen`, `group`, `alias`), all 18 standard context fields, structured Client Hints (`context.userAgentData`), 18 reserved identify traits, 12 reserved group traits, and seven semantic event categories (E-Commerce v2, Video, Mobile, B2B SaaS, Email, Live Chat, A/B Testing). RudderStack extensions beyond the Segment spec — including `/v1/replay`, `/internal/v1/retl`, `/beacon/v1/*`, `/pixel/v1/*`, and the `merge` call type — are documented in the [Event Spec API Reference](docs/api-reference/event-spec/). The analysis also covers destination catalog coverage, transformation/Functions, Protocols enforcement, identity resolution, and warehouse sync.

> **Note:** Segment Engage/Campaigns and Reverse ETL are planned for Phase 2.

## Contribute

We would love to see you contribute to RudderStack. Get more information on how to contribute [**here**](https://github.com/rudderlabs/rudder-server/blob/master/CONTRIBUTING.md).

## License

RudderStack server is released under the [**Elastic License 2.0**](LICENSE).

