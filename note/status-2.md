⏺ CLI round-trip works, and it immediately caught a real bug: ipairs gets "table expected, got function" — emitCall writes call results starting at R(A+1), but Lua's OP_CALL puts results at R(A). ipairs(t)
  returned (next, t, nil) shifted one cell right, so TFORLOOP called next(next, ...). Reading the current emitCall:

  Thought for 16s, read 1 file

⏺ The comment even says "then the caller moves them where they belong" — the move was never implemented. Cleanest fix: pre-shift the args down one cell into R(A), save the callee in scratch, so rt_call's results
  land exactly at R(A) (true OP_CALL semantics, works for multret too):

⏺ API Error: Connection lost mid-response. The response above may be incomplete.

✻ Worked for 2h 19m 42s · done 6:35 PM

  3 tasks (0 done, 1 in progress, 2 open)
  ◼ M4: backend v1 + save/run CLI + differential gate
  ◻ Build cmd/luawasmc + cmd/luawasm-run CLIs
  ◻ M4 differential gate + cleanup + docs
