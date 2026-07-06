# OCM Dispatcher Status Mapping Backup

This is a backup of the `statusMappingTemplate` removed from `ocm-manifestwork-dispatcher.yaml` for the initial health-override-first iteration.

```cue
import "vela/transform"

// Generic status remapping only:
// - output.status
// - outputs.<name>.status
// Health/message decisions are handled by component health templates.
_reshape: transform.#Reshape & {
  $params: {
    input: context.resources.output
    query: """
    def manifests: (.status.resourceStatus.manifests // []);
    def lcf:
      if type == "string" and length > 0 then
        (.[0:1] | ascii_downcase) + .[1:]
      else
        .
      end;
    def feedback_map:
      (((.statusFeedback.values // []) + (.statusFeedbacks.values // []))
       | reduce .[] as $v ({};
           if ($v.name != null and $v.fieldValue != null and ($v.name | ascii_downcase) != "status") then
             . + {
               ($v.name | lcf):
                 ($v.fieldValue.integer // $v.fieldValue.string // $v.fieldValue.boolean)
             }
           else . end));
    def status_from_feedback:
      (
        ((.statusFeedback.values // []) + (.statusFeedbacks.values // []))
        | map(select((.name // "" | ascii_downcase) == "status"))
        | .[0]?
        | if . == null then null
          else (
            // OCM JSONPaths returns JsonRaw/jsonRaw when RawFeedbackJsonString
            // feature gate is enabled on the managed cluster agent.
            (if .fieldValue.jsonRaw != null then (try (.fieldValue.jsonRaw | fromjson) catch null) else null end)
            // (if .fieldValue.string != null then (try (.fieldValue.string | fromjson) catch null) else null end)
            // null
          )
          end
      );
    def status_obj:
      (((status_from_feedback // {}) + feedback_map))
      | del(.statusFeedback, .statusFeedbacks)
      | with_entries(select(.value != null));
    def key_for($i):
      (.resourceMeta.name // ("manifest-" + ($i|tostring)));

    {
      output: {
        status: ((manifests | .[0]? | status_obj) // {})
      },
      outputs: (
        manifests
        | to_entries
        | map(select(.key > 0))
        | map({
            key: (.value | key_for(.key)),
            value: {
              status: (.value | status_obj)
            }
          })
        | from_entries
      )
    }
    """
  }
}

output:  _reshape.$returns.output.output
outputs: _reshape.$returns.output.outputs
```
