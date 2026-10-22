# Shell test harness

Run shell checks with explicit input and keep test diagnostics separate from
production output. The pattern below leaves the useful failure status intact:

```sh
if ! ./scripts/check.sh; then
  echo "Check failed; preserve the original exit status." >&2
  exit 1
fi
```

Do not suppress a failure just to make the test suite green.
