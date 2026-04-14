# Hugr Aggregation Functions

## Count

```graphql
{
  module_name {
    table_name_aggregate {
      aggregate {
        count
      }
    }
  }
}
```

## Count with Filter

```graphql
{
  module_name {
    table_name_aggregate(where: {status: {_eq: "active"}}) {
      aggregate {
        count
      }
    }
  }
}
```

## Sum, Avg, Min, Max

```graphql
{
  module_name {
    table_name_aggregate {
      aggregate {
        sum { amount }
        avg { amount }
        min { created_at }
        max { created_at }
      }
    }
  }
}
```

## Group By

```graphql
{
  module_name {
    table_name_aggregate(group_by: [category]) {
      group_key { category }
      aggregate {
        count
        avg { price }
      }
    }
  }
}
```
