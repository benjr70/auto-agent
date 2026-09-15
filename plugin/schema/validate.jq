# Validate an instance against the subset of JSON Schema draft-07 that
# harness.schema.json uses, with jq alone (ADR 0002: jq is the only parser).
#
# Usage: jq -r --slurpfile schema harness.schema.json -f validate.jq harness.json
# Output: one error per line as "<json-pointer>: <message>"; nothing when valid.
#
# Supported keywords: type, required, properties, additionalProperties,
# propertyNames, enum, const, items, minItems, minimum, maximum, minLength,
# pattern, oneOf, anyOf, allOf, not. Unknown keywords are ignored, so the
# schema stays a normal draft-07 document that ajv or python-jsonschema can
# also read.

def jtype:
  if type == "number" then (if . == floor then "integer" else "number" end) else type end;

def type_ok($t):
  ($t == "number" and (type == "number"))
  or ($t == "integer" and (jtype == "integer"))
  or ($t == jtype)
  or ($t == "object" and type == "object")
  or ($t == "array" and type == "array")
  or ($t == "string" and type == "string")
  or ($t == "boolean" and type == "boolean")
  or ($t == "null" and type == "null");

# validate(instance; schema; pointer) -> stream of error strings
def validate($inst; $s; $p):
  if ($s | type) == "boolean" then
    (if $s then empty else "\($p): not allowed" end)
  else
    (
      if $s.type? then
        ($s.type | if type == "array" then . else [.] end) as $ts
        | if any($ts[]; . as $t | $inst | type_ok($t)) then empty
          else "\($p): expected \($ts | join(" or ")), got \($inst | jtype)" end
      else empty end
    ),
    (
      if $s.enum? then
        if ($s.enum | index([$inst])) != null then empty
        else "\($p): must be one of \($s.enum | map(tojson) | join(", ")), got \($inst | tojson)" end
      else empty end
    ),
    (
      if $s | has("const") then
        if $inst == $s.const then empty else "\($p): must equal \($s.const | tojson)" end
      else empty end
    ),
    (
      if ($inst | type) == "object" then
        (
          ($s.required // [])[] as $r
          | if $inst | has($r) then empty else "\($p): missing required key \"\($r)\"" end
        ),
        (
          ($s.properties // {}) | to_entries[] as $e
          | if $inst | has($e.key) then validate($inst[$e.key]; $e.value; "\($p)/\($e.key)") else empty end
        ),
        (
          $inst | keys[] as $k
          | if (($s.properties // {}) | has($k)) then empty
            elif ($s | has("additionalProperties")) then
              if ($s.additionalProperties | type) == "boolean" then
                (if $s.additionalProperties then empty else "\($p): unknown key \"\($k)\"" end)
              else validate($inst[$k]; $s.additionalProperties; "\($p)/\($k)") end
            else empty end
        ),
        (
          if $s.propertyNames? then
            $inst | keys[] as $k
            | validate($k; $s.propertyNames; "\($p)/\($k)")
          else empty end
        )
      else empty end
    ),
    (
      if ($inst | type) == "array" then
        (
          if $s.minItems? and ($inst | length) < $s.minItems then
            "\($p): needs at least \($s.minItems) item(s)"
          else empty end
        ),
        (
          if $s.items? then
            range($inst | length) as $i | validate($inst[$i]; $s.items; "\($p)/\($i)")
          else empty end
        )
      else empty end
    ),
    (
      if ($inst | type) == "number" then
        (if ($s | has("minimum")) and $inst < $s.minimum then "\($p): must be >= \($s.minimum)" else empty end),
        (if ($s | has("maximum")) and $inst > $s.maximum then "\($p): must be <= \($s.maximum)" else empty end)
      else empty end
    ),
    (
      if ($inst | type) == "string" then
        (if $s.minLength? and ($inst | length) < $s.minLength then "\($p): must be at least \($s.minLength) character(s)" else empty end),
        (if $s.pattern? and (($inst | test($s.pattern)) | not) then "\($p): \"\($inst)\" does not match \($s.pattern)" else empty end)
      else empty end
    ),
    (
      if $s.allOf? then $s.allOf[] as $sub | validate($inst; $sub; $p) else empty end
    ),
    (
      if $s.anyOf? then
        if any($s.anyOf[]; . as $sub | ([validate($inst; $sub; $p)] | length) == 0) then empty
        else "\($p): matches none of the allowed shapes" end
      else empty end
    ),
    (
      if $s.oneOf? then
        ([$s.oneOf[] | . as $sub | select(([validate($inst; $sub; $p)] | length) == 0)] | length) as $n
        | if $n == 1 then empty
          elif $n == 0 then "\($p): matches none of the allowed shapes (\($s.oneOf | map(.required // [] | join("+")) | join(" | ")))"
          else "\($p): matches more than one shape; declare exactly one of \($s.oneOf | map(.required // [] | join("+")) | join(" | "))" end
      else empty end
    ),
    (
      if $s | has("not") then
        if ([validate($inst; $s.not; $p)] | length) == 0 then "\($p): must not match the forbidden shape" else empty end
      else empty end
    )
  end;

validate(.; $schema[0]; "")
| sub("^: "; "/: ")
