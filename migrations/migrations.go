// Package migrations contains SQL schema migrations split by scope:
//
//   - system/: applied once to the public schema (tenants, users, memberships)
//   - tenant/: applied to every tenant_<slug> schema (domain tables)
//
// This file only documents the layout; the actual SQL lives in the
// subdirectories so goose can track each set independently.
package migrations
