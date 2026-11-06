# go-gardening-demo

An elaborate demo of go-proxy: a gardening-equipment online store exposed
through MCP tools, resources, and prompts. This is step 1 — the seed data
the rest of the demo will be built on. Source spec: the original request
(reproduced faithfully below, typos and all, since it's the ground truth
this generator implements).

## What's here

- `initial-data/*.json` — one JSON file per entity, generated data.
- `gendata/` — the Go program that generates `initial-data/`. Deterministic
  (fixed rng seed), so re-running it reproduces byte-identical output:

  ```
  cd gendata && go run . ../initial-data
  ```
- `servers/` — the 5 `garden-*` MCP backend servers exposing this data through go-proxy.
- `cli/` — the manual-operation CLI for those servers; `cli.sh` (this directory) is a thin
  wrapper around `go run ./go-gardening-demo/cli` so it can be run from anywhere without
  `cd`-ing into the repo root first, e.g.:

  ```
  go-gardening-demo/cli.sh -username admin -password admin init
  ```

## Entities

| File | Count | Notes |
|---|---|---|
| `product_supply.json` | 50 rows / 30 unique products | up to 3 suppliers/product |
| `supply_orders.json` | 100 | all fulfilled |
| `customer_types.json` | 3 | lookup: one-time-customer, returning-customer, vip |
| `customer_sentiments.json` | 3 | lookup: neutral, happy, unhappy |
| `online_customers.json` | 300 | 50% returning, 5% VIP, 45% one-time |
| `online_customer_orders.json` | 500 | 10% (the 50 most recent) not yet fulfilled |
| `staff_roles.json` | 10 | see "Staffer headcount and roles" below |
| `staffers.json` | 14 | see "Staffer headcount and roles" below |
| `customer_service_calls.json` | 30 | |
| `deals.json` | 4 | one per season |
| `client_access_rules.json` | 9 | per-role tool grants, see "Role-based access control" below |

`product_supply.json`'s `productId` is the cross-entity product identity
(30 unique values); its own `id` is unique per supplier row (50 values) —
`online_customer_orders.json` references the former, since a customer
orders a product, not a specific supplier's batch. `deals.json` references
the latter instead (its `ids` field): a deal can single out one supplier's
row rather than automatically covering every supplier of a product, so
matching an order's discount against a deal has to resolve each deal `id`
back to its `productId` first (see `gendata/build.go`'s `activeDeal`).

An `online_customer_orders.json` entry carries a `dealId` (and a discounted
`totalPrice`) whenever it was placed for a product, on a date, that a Deal
covered — capped in code at no more than 25% of all orders
(`gendata/build.go`'s `maxDealOrders`), regardless of how much the random
order dates happen to overlap the 4 deal windows. Currently well under the
cap (~6%): most orders simply don't land inside a ~3-week deal window for
a covered product.

## Assumptions made where the spec was silent or self-contradictory

- **Directory name**: the spec says `initial-date`; every other typo in
  the source doc (`gardeining`, `Fullfilment`, `Cusomer`, `Proct`, ...) is
  an unambiguous misspelling of a real word, and `initial-data` is what
  the surrounding text ("Create a json file for each entity... Populating
  the JSON files") is clearly describing, so that's what got created.
- **Staffer headcount and roles**: a 9-role catalog (`admin`,
  `business-manager`, `customer-support-manager`, `customer-management`,
  `customer-support`, `human_resource_manager`, `human_resource_staffer`,
  `inventory-clerk`, `business-staff`) held by 14 staffers, matching the
  staffer-roles spec's headcount table exactly — see
  `gendata/staticdata.go`'s `staffers` for the full roster. `admin` is
  listed in `staff_roles.json` on equal footing with every other role. The
  spec's "cutomer-support" (used in its per-tool access lists) and
  "customer_support" (used once in its headcount) are the same role,
  canonicalized to `customer-support`.
- **`deactivated` role**: not in the spec's role list — a staffer
  holding it keeps their functional role(s) (role
  history isn't erased) but `buildCustomerServiceCalls` (`gendata/build.go`)
  excludes anyone holding it from the pool a new `customer_service_calls.json`
  entry can be assigned to. No staffer currently holds it, so this
  currently has no visible effect on `staffers.json`/
  `customer_service_calls.json` — the role and the exclusion logic both
  still exist for whenever it's assigned to someone again.
- **Role-based access control (`client_access_rules.json`, per the
  staffer-roles spec)**: `gendata/accessrules.go`'s
  `buildClientAccessRules` inverts that spec's per-tool "which roles can
  call this" lists into one `access_control.AccessibilityRule` per role (9
  rules — `admin` excluded, since `proxy-server.cfg`'s own `{"role":
  "admin", "allowed_tools": ["*"], ...}` rule already covers it), so each
  role can call only the `garden-*` tools the spec grants it. `cli init`
  loads it automatically (its
  last step, policy after data) via `admin update_access_rules`
  (`cli/admin.go`), which fetches the gateway's live rules via
  `_admin.export_role_permissions`, drops any existing rule whose
  `allowed_tools` are *all* `garden-*`-namespaced, appends the new 9, and
  writes the result back via `_admin.update_role_permissions` — a merge,
  not a wholesale replace, so it never disturbs the admin role's own rule
  or (in `proxy-server.cfg`) the separate `proxy_utility.call_tool` grant.
  Also runnable standalone: `cli admin update_access_rules -file
  go-gardening-demo/initial-data/client_access_rules.json`.
- **Fields not in the spec's per-entity field lists, there because
  "entities relations are logical" requires them**: `CustomerServiceCall`
  has `customerId`/`handledByStafferId` (a call floating with no caller
  or handler isn't a logical relation), `OnlineCustomerOrder` has
  `dealId` (traceability for which Deal, if any, discounted an order), and
  `Deal` has `name` (a deal needs a human-readable label to be usable
  at all).
- **Sentiment distribution for the 300 online customers**: the spec
  enumerates the 3 sentiment values but never gives proportions the way it
  does for customer type (50%/5%). Assumed 45% neutral / 35% happy / 20%
  unhappy.
- **Order-placement weighting**: VIP and returning customers are weighted
  3x and 2x respectively over one-time customers when assigning
  `online_customer_orders.json`'s `customerId` — not specified, but a
  reasonable reading of what "returning"/"VIP" ought to mean for order
  volume.
- **Deal seasons/products/discounts**: the spec says "4 deals at different
  times of the year" with no further detail. Picked one deal per season
  (Fall Cleanup Clearance, Winter Prep Bundle, Spring Planting Sale, Summer
  Watering Savings), each scoped to a thematically-relevant slice of the
  catalog, at 15–25% off.
- **Dataset date window**: all dates fall within `2025-08-15`–`2026-08-15`
  (see `gendata/main.go`'s `datasetStart`/`referenceToday`) — a ~12-month
  window ending near this repo's current date, wide enough to fit all 4
  seasonal deals.
- **Catalog price is per-product, not per-supplier-row**:
  `ProductSupply.costToUserPerRetailUnit` is identical across every
  supplier row for the same product (a store doesn't usually vary its
  shelf price by which supplier happened to fulfill the current stock),
  and is set high enough to stay profitable against whichever of that
  product's suppliers has the highest per-unit wholesale cost — not just
  an arbitrary one of them. (The "user" in this field's name refers
  generically to the retail buyer, not to the `OnlineCustomer` entity.)
- **Retail margin is a realistic 20–50% over the worst-case supplier's
  per-unit cost** (`gendata/build.go`'s `markup`, 1.2x–1.5x), since these
  prices are exposed through real purchase/pricing MCP tools.
  `gendata/util.go`'s `roundToNinetyNine` rounds to the *nearest* `X.99`
  price point rather than always rounding *up* to the next one, since
  always-up rounding can nearly double a cheap per-unit cost on its own
  (e.g. $0.60 → $0.99), independent of and stacking with whatever markup
  was applied, pushing some products' effective margin outside the
  intended band.
- **"customer" naming**: the store's buyers are `OnlineCustomer`/
  `CustomerType`/`CustomerSentiment`/`OnlineCustomerOrder`
  (`online_customers.json`, `customer_types.json`,
  `customer_sentiments.json`, `online_customer_orders.json`), consistent
  with `CustomerServiceCall`'s and the staff roles' "customer"
  terminology. `OnlineCustomer.id` values are `cust-000N`, and every
  field/id that references an online customer
  (`CustomerServiceCall.customerId`, `OnlineCustomerOrder.customerId`,
  `customer_types.json`'s `one-time-customer`/`returning-customer` ids)
  uses that naming.
- **Known, harmless artifact**: a small number of
  `online_customer_orders.json` totals may be a single cent off from an
  independent recomputation done with a different rounding mode (Go's
  `math.Round`, round-half-away-from-zero, vs. e.g. Python's
  round-half-to-even) landing on opposite sides of an exact `x.xx5`
  boundary. The generated value is internally consistent (computed with
  one rounding function throughout); this is a rounding-mode artifact, not
  a data error.
