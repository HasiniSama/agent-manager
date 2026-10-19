# wso2-amp-observability-extension

Installs the Agent Manager Observer into the OpenChoreo observability plane and, optionally, the score store: the Postgres that holds per-trace evaluation scores.

The chart depends on the Bitnami `postgresql` chart. Fetch it before rendering or installing from a checkout:

```sh
helm dependency build deployments/helm-charts/wso2-amp-observability-extension
```

## Score store

Off by default (`scoreStore.enabled: false`). With it off, the chart renders exactly what it rendered before the score store existed.

The store has two tables, `trace_scores` and `span_scores`, and three logins:

| Login | Used by | Can do |
|---|---|---|
| admin | the migration Job only | owns the schema, the roles and the policies |
| `amp_scores_writer` | the score writer | `SELECT`, `INSERT`, `UPDATE`, `DELETE` on the two tables, for every org |
| `amp_scores_reader` | the observer | `SELECT` on the two tables, only the rows of the org its transaction sets in `amp.org_id`; with the setting unset or empty it sees nothing |

Every TCP connection uses TLS, and clients verify the server certificate and hostname (`sslmode=verify-full`).

### Self-hosted: the bundled Postgres

```sh
helm upgrade --install wso2-amp-observability-extension \
  deployments/helm-charts/wso2-amp-observability-extension \
  --namespace openchoreo-observability-plane \
  --set scoreStore.enabled=true
```

This installs:

- **Postgres.** The Bitnami `postgresql` chart, version 18.1.13 (the version `wso2-agent-manager` uses), as `amp-score-store-postgresql`, with database `amp_scores`. Its `pg_hba.conf` accepts TCP connections over TLS only (`hostssl`). Configure it under `scoreStorePostgresql`, using the Bitnami chart's own keys.
- **A CA and a server certificate**, from cert-manager, which must be installed. The chart creates its own CA in the release namespace. The CA certificate is in the Secret named by `scoreStore.tls.caSecret` (default `amp-score-store-ca`). Clients mount only that Secret's `ca.crt` key, because the Secret also holds the CA's private key. Postgres reads its certificate when it starts, so restart `amp-score-store-postgresql-0` after cert-manager renews it (30 days before its 5-year expiry).
- **Three Secrets with generated passwords**, each under key `password`: `amp-score-store-admin`, `amp-score-store-writer` and `amp-score-store-reader`. An upgrade keeps the stored values. The admin Secret is kept on uninstall, because the data volume keeps that password. Tools that render without cluster access, such as Argo CD, cannot read the stored values, and would generate new passwords on every sync. Use the external mode with your own Secrets there.
- **A NetworkPolicy** that admits connections on 5432 only from pods labelled `app: amp-observer`, `app: amp-score-writer` or `app: amp-score-store-migration` in the release namespace.
- **The migration Job** (below).

Requests are 256Mi and 250m, limits 512Mi and 500m. The volume is 10Gi.

### External or managed Postgres

```sh
helm upgrade --install wso2-amp-observability-extension \
  deployments/helm-charts/wso2-amp-observability-extension \
  --namespace openchoreo-observability-plane \
  --set scoreStore.enabled=true \
  --set scoreStore.postgresql.enabled=false \
  --set scoreStore.external.host=scores.db.example.com \
  --set scoreStore.external.caSecret=score-db-ca \
  --set scoreStore.external.adminSecret=score-db-admin \
  --set scoreStore.external.writerSecret=score-db-writer \
  --set scoreStore.external.readerSecret=score-db-reader
```

Create the Secrets in the release namespace first:

| Value | Secret keys | Holds |
|---|---|---|
| `external.caSecret` (required) | `ca.crt` | the CA that signed the server's certificate |
| `external.adminSecret` | `username`, `password` | a login that owns the database and can create roles |
| `external.writerSecret` | `password` | the password to set on `amp_scores_writer` |
| `external.readerSecret` | `password` | the password to set on `amp_scores_reader` |

- **TLS.** `external.sslMode` accepts only `verify-full`, so `external.host` must match the server certificate.
- **The database.** `external.database` (default `amp_scores`) must already exist. It can be a separate database on an existing server.
- **Without the admin login.** Leave `external.adminSecret` empty and the chart runs no migration Job. Apply the files in `files/score-store/migrations` to the database yourself, in name order, as a login that owns it. Then give both roles `LOGIN` and the passwords in the writer and reader Secrets.
- **cert-manager is still required.** The chart's own CA is created in this mode too.

### Choreo cloud

Use the external mode against the Postgres that cloud operations manages. They provide:

- a Postgres reachable from the observability cluster, with TLS and a certificate for its hostname;
- the CA that signed it, as the `ca.crt` of `external.caSecret`;
- either an admin login for the migration Job, or the schema and the two roles applied from `files/score-store/migrations`;
- the writer and reader passwords, as `external.writerSecret` and `external.readerSecret`.

The cloud's trace retention must be confirmed before setting `scoreStore.retentionDays` (see Retention).

### The migration Job

`amp-score-store-migration` runs after every install and upgrade:

1. It waits up to 5 minutes for the database to accept a TLS connection with the admin login.
2. It applies each file in `files/score-store/migrations` in name order, each in its own transaction. Applied versions are recorded in `schema_migrations`. A file already recorded there is skipped, so running the Job again changes nothing.
3. It gives `amp_scores_writer` and `amp_scores_reader` `LOGIN` with the passwords from their Secrets. A role whose Secret is missing stays without login. A role that already logs in with its Secret is left alone.

It needs:

- the admin login;
- the CA Secret;
- network access to the database. The bundled NetworkPolicy admits the Job by its `app: amp-score-store-migration` label.

Its image (`scoreStore.migration.image`, default `postgres:18-alpine`) must provide psql 15 or later. No other workload mounts the admin Secret.

The same SQL is kept in `agent-manager-observer/scorestore/migrations` for the services' tests. A test there fails if the two copies differ.

### Score writer

`amp-score-writer` runs from the observer's image (`/go/bin/score-writer`) whenever the score store is on. In external mode it needs `external.writerSecret`; without it the chart skips the writer. It logs in as `amp_scores_writer` with `sslmode=verify-full` against the score store CA, and listens on two TLS ports with a certificate from that CA (`amp-score-writer-tls`, read at start):

| Port | Value | Callers | Serves |
|---|---|---|---|
| 9099 | `scoreWriter.publishPort` | evaluation jobs in `scoreWriter.networkPolicy.publishNamespaces`, and the gateway | `POST /api/v1/publisher/monitors/{monitorId}/runs/{runId}/scores`, run tokens only (see `agent-manager-observer/docs/score-writer-openapi.yaml`) |
| 9199 | `scoreWriter.internalPort` | agent-manager-service's namespace (`scoreWriter.networkPolicy.internalNamespace`) only | `GET /healthz`, and `GET /readyz`, which answers 200 once the writer login reaches the database |

- **Gateway.** With `scoreWriter.ocIngress.enabled`, an HTTPRoute sends `scoreWriter.ocIngress.hostname` (default `scores.amp.localhost`), path `/api/v1/publisher/`, to port 9099 only, and a kgateway `BackendConfigPolicy` makes the gateway connect over TLS and verify the writer's certificate. The default `gateway-default` listener is plain HTTP; for jobs in another cluster, point `ocIngress.gatewayName` at a Gateway with an HTTPS listener.
- **NetworkPolicy.** Ingress as in the table. Egress only to DNS, the bundled Postgres, Thunder (`networkPolicy.idp`) and agent-manager-service (`networkPolicy.runTokenIssuer`). For an external database, add an `ipBlock` rule to `networkPolicy.extraEgress`.
- **Keys.** `scoreWriter.runToken` points at agent-manager-service's run-token key set, fetched on the first publish and again on an unknown key ID at most once a minute (until they first load, publishes get 503 so the job retries); `scoreWriter.internalAuth` at Thunder's, with the issuer falling back to `amObserver.auth.issuer`.
- **Retention.** The writer refuses a score whose trace started before `scoreStore.retentionDays`.

### Retention

`scoreStore.retentionDays` (default 30) is how long a score is kept. **It must equal the tracing module's retention.** OpenChoreo's default is 30 days. If the score retention is shorter, scores disappear while their traces are still visible. If it is longer, the store keeps scores for traces that no longer exist.

### Sizing

Measured on the bundled Postgres, with all indexes:

- each trace row takes about 1.1 KB, with a 400-character explanation;
- each scored span takes about 0.6 KB, with a 300-character explanation.

Estimate the volume as traces scored per day × evaluators × `retentionDays` × (1.1 KB + 0.6 KB × scored spans per trace). For example, 10,000 traces a day, 3 evaluators and 2 scored spans each come to about 2 GB over 30 days. Set `scoreStorePostgresql.primary.persistence.size` from that estimate.
