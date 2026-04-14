# Hugr Query Patterns

## Basic Query

```graphql
{
  module_name {
    table_name(limit: 10) {
      field1
      field2
    }
  }
}
```

## Query with Filter

```graphql
{
  module_name {
    table_name(where: {field1: {_eq: "value"}}) {
      field1
      field2
    }
  }
}
```

## Query with Sorting

```graphql
{
  module_name {
    table_name(order_by: {field1: asc}, limit: 20) {
      field1
      field2
    }
  }
}
```

## Nested Query (Joins)

```graphql
{
  module_name {
    parent_table {
      id
      name
      child_table {
        id
        value
      }
    }
  }
}
```

## Pagination

```graphql
{
  module_name {
    table_name(limit: 10, offset: 20) {
      field1
      field2
    }
  }
}
```
