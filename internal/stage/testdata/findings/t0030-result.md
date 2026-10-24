## Confirmed bugs (all run against current main; scripts in scratch only)

### 1. Hex encode/decode loses leading zeros and empty strings (data loss, medium)
```ruby
r = IdCodec::Reversible.tinyids   # same for saltid
r.decode_hex(r.encode_hex("00ff"))        # => ["ff"]
r.decode_hex(r.encode_hex(["", "ab"]))    # => ["ab"]  (element dropped)
```
Impact: hex values with leading zeros don't round-trip. Fix ticket: *"Preserve hex string length in encode_hex/decode_hex round trip"*.

### 2. `decode(nil)` / non-String input raises NoMethodError (low)
```ruby
IdCodec::Reversible.tinyids.decode(nil)  # NoMethodError undefined method `length' for nil
```
Impact: callers rescuing `IdCodecFormatError` get a 500. Fix ticket: *"Raise IdCodecFormatError for nil/non-String input in decode and decode_hex"*.

### 3. Saltid with multibyte alphabet: encode works, decode crashes (medium)
```ruby
x = IdCodec::Reversible.saltid(salt: "abcd", alphabet: IdCodec::Alphabet.new("àáâãäåæçèéêëìíîï"))
x.decode(x.encode(77))  # Encoding::CompatibilityError
```
Fix ticket: *"Fix or reject multibyte alphabets in the Saltid encoder"*.

### 4. Tinyids encode of integers > 2**62-1 raises ArgumentError (low/medium)
```ruby
IdCodec::Reversible.tinyids.encode(2**64) # ArgumentError
```
Fix ticket: *"Wrap out-of-range Tinyids encode errors in InvalidInputError"*.

### 5. Non-numeric/nil values are silently encoded as 0 (medium)
```ruby
r.encode(nil) == r.encode("abc") # true, both encode [0]
```
A missing id yields a valid-looking ID for 0. Fix ticket: *"Reject nil and non-integer-like values in encode"*.

### 6. Inconsistent handling of invalid strings between encoders / methods (low)
```ruby
IdCodec::Reversible.tinyids.decode("!!!!")              # => []
IdCodec::Reversible.saltid(salt: "abcd").decode("!!!!") # IdCodecFormatError
```
Fix ticket: *"Make decode/decode_hex error behaviour consistent across encoders"* (the decode_hex part is already in open PR #27).

## Open GitHub issues checked
Open issues: #18 (cache coder; PR #26 open), #16, #15 (thread safety), #11. None reproduce besides item 6 (PR #27).

## Probed and fine
- Test suite: 535 runs, 0 failures.
- 3000 random round trips each for tinyids and saltid: no mismatches.
