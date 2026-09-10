⏺ Everything is checkpointed for a clean restart. Safe to /exit whenever you're ready.                    
                                                                                                          
  What's saved for the next session
                                                                                                          
  note/m4-to-m5-plan.md — a fresh checkpoint section prepended at the top (supersedes the old "open trap" 
  section), with:                                                                                         
  - The three root-cause fixes from this session: intern-pointer cursor-len (every string constant was    
    interning garbage — the real cause behind both the zz and print mysteries), the argcells=&R(A+1) call 
    fix, and the rt_setglobal double-pop.
  - The open bug with complete evidence: print(42) → PRINT nil; bytecode, emitted disassembly, and        
    runtime diagnostics all documented — the emitted code provably stores 42 into frame+16, yet rt_call   
    reads that cell as nil.                                                                               
  - The first next step written out: dump kcells for the print(42) module after init (extend              
    testdiff/calldiag_test.go using the existing zz2/kcell machinery). Prime suspect: the init value-loop 
    mis-sequencing for mixed string/number Constants — ["print", 42] — since the dump that verified
    cell[1]=tag3 was from the zz=5 module, not this one.                                                  
  - The queued FORLOOP bug (~36 iterations for range 3, then stack-overflow trap — back-edge block        
    mapping), the full mini-matrix results, and the list of debug exports/tests to delete at M4 cleanup.  
                                                                                                          
  Project memory updated with the same, so just opening the repo and saying "continue the M4 backend      
  debugging" (or "resume from the note") will pick up exactly at that kcells dump.
                                                                                                          
  Tree state: builds clean; nothing committed (as you've preferred throughout); the runtime blob carries  
  the diagnostic exports.                                                                                 
                                                                                                          
  When you're back: cd into the repo, start Claude Code, and point it at the note. Good luck with the     
  last stretch of this bug — the evidence trail is narrow enough that it should fall quickly.
                                                                                                          
✻ Sautéed for 25m 57s · done 3:41 PM                                                                      

  1 tasks (0 done, 1 in progress, 0 open)
  ◼ M4: backend v1 + save/run CLI + differential gate

──────────────────────────────────────────────────────────────────────────────────────────────────────────

❯ Continue with the print(42) bug
