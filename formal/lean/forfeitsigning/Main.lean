import ForfeitSigning

open ForfeitSigning

/-- renderCase emits the stable tab-separated bridge format. -/
def renderCase (test : BridgeCase) : String :=
  s!"{test.name}\t{if test.accepted then "accept" else "reject"}"

/-- main emits decisions computed by the same admission function we prove. -/
def main : IO Unit :=
  bridgeCases.forM fun test => IO.println (renderCase test)
