package request

// MaxHandedOverSpecBytes is the maximum size of an operator-shaped spec
// accepted at submission, through the console editor, or after an imported
// spec is redrafted. It is 120,000 bytes. The complete ticket build input has
// its own 180,000-byte cap, leaving 20,000 bytes below build_app.py's
// 200,000-byte round-prompt ceiling for round state and corrective framing;
// the smaller spec cap leaves room for the ticket and criteria framing too.
const MaxHandedOverSpecBytes = 120_000
