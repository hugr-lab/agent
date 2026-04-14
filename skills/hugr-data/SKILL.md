---
name: hugr-data
description: Explore and query the Hugr data mesh — discover modules, inspect schemas, build GraphQL queries, present results
categories:
  - data
  - hugr
  - exploration
---

# Hugr Data Exploration

You have access to the Hugr data mesh through MCP tools. Use them to discover data sources, inspect schemas, build queries, and present results to the user.

## Workflow

1. **Discovery**: Start with `discover_modules` or `search` to find relevant data sources.
2. **Schema inspection**: Use `get_schema` or `get_fields` to understand table structure before querying.
3. **Query building**: Construct GraphQL queries using the schema information. Always validate before executing.
4. **Execution**: Run queries with `execute_query`. Prefer aggregations over raw data dumps.
5. **Presentation**: Format results clearly. Use jq transforms (`jq_query`) to reshape data when needed.

## Key Rules

- ALWAYS inspect schema before building queries.
- ALWAYS validate queries before executing.
- Prefer aggregations (count, sum, avg) over SELECT * dumps.
- Use filters to narrow results — don't fetch everything.
- If a query fails, check the error, fix the query, and retry.
- For large datasets, use LIMIT and OFFSET.

## References

- **query-patterns**: Common GraphQL query building patterns for Hugr
- **aggregations**: Aggregation functions, grouping, and statistical operations
- **filters**: Filter expressions, operators, and combining conditions
