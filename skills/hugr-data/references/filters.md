# Hugr Filter Expressions

## Comparison Operators

| Operator | Description | Example |
|----------|-------------|---------|
| `_eq` | Equal | `{status: {_eq: "active"}}` |
| `_neq` | Not equal | `{status: {_neq: "deleted"}}` |
| `_gt` | Greater than | `{age: {_gt: 18}}` |
| `_gte` | Greater or equal | `{age: {_gte: 18}}` |
| `_lt` | Less than | `{price: {_lt: 100}}` |
| `_lte` | Less or equal | `{price: {_lte: 100}}` |
| `_in` | In list | `{status: {_in: ["active", "pending"]}}` |
| `_nin` | Not in list | `{status: {_nin: ["deleted"]}}` |
| `_is_null` | Is null | `{email: {_is_null: true}}` |
| `_like` | SQL LIKE | `{name: {_like: "%john%"}}` |
| `_ilike` | Case-insensitive LIKE | `{name: {_ilike: "%john%"}}` |

## Logical Operators

### AND (implicit)

```graphql
where: {status: {_eq: "active"}, age: {_gt: 18}}
```

### OR

```graphql
where: {_or: [{status: {_eq: "active"}}, {status: {_eq: "pending"}}]}
```

### NOT

```graphql
where: {_not: {status: {_eq: "deleted"}}}
```

## Combining Conditions

```graphql
where: {
  _and: [
    {status: {_eq: "active"}},
    {_or: [
      {category: {_eq: "premium"}},
      {age: {_gt: 25}}
    ]}
  ]
}
```
