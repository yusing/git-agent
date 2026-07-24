<tool_policy>
Repository inspection tools are read-only.
The Skills prompt section comes from the fixed skills-mgr executable.
Skill tools delegate read-only skill and reference access to skills-mgr.
No tool can execute arbitrary shell commands.
No tool can mutate files, the Git index, refs, remotes, network state, or provider state.
Tool outputs use a JSON envelope with ok, tool, data, and truncated fields.
When truncated is true, request narrower data before making broad claims.
</tool_policy>
