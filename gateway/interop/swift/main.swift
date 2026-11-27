// Interop CLI: seal / opens2c / selftest. See Makefile target `interop`.
import Foundation
import CryptoKit

func fail(_ m: String) -> Never {
    FileHandle.standardError.write(Data(("FAIL: " + m + "\n").utf8))
    exit(1)
}

func unhex(_ s: String) -> Data {
    guard s.count % 2 == 0 else { fail("bad hex") }
    var d = Data(); var i = s.startIndex
    while i < s.endIndex {
        let j = s.index(i, offsetBy: 2)
        guard let b = UInt8(s[i..<j], radix: 16) else { fail("bad hex") }
        d.append(b); i = j
    }
    return d
}
func hex(_ d: Data) -> String { d.map { String(format: "%02x", $0) }.joined() }

func loadJSON(_ path: String) -> [String: Any] {
    guard let d = FileManager.default.contents(atPath: path),
          let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { fail("cannot read \(path)") }
    return o
}
func str(_ o: [String: Any], _ k: String) -> String {
    guard let s = o[k] as? String else { fail("missing \(k)") }
    return s
}

struct Vec {
    let vin: String, hello: Data, challenge: Data
    let devSK: P256.KeyAgreement.PrivateKey
    let pkE: P256.KeyAgreement.PublicKey
    let gwPK: P256.Signing.PublicKey
}
func loadVec(_ path: String) -> Vec {
    let o = loadJSON(path)
    do {
        return Vec(vin: str(o, "vin"), hello: unhex(str(o, "hello")), challenge: unhex(str(o, "challenge")),
                   devSK: try P256.KeyAgreement.PrivateKey(rawRepresentation: unhex(str(o, "device_sk"))),
                   pkE: try P256.KeyAgreement.PublicKey(x963Representation: unhex(str(o, "ephemeral_pk"))),
                   gwPK: try P256.Signing.PublicKey(x963Representation: unhex(str(o, "gateway_pk"))))
    } catch { fail("key load: \(error)") }
}

let args = CommandLine.arguments
guard args.count >= 2 else { fail("usage: interop selftest|seal|opens2c ...") }

switch args[1] {
case "selftest":
    guard args.count == 3 else { fail("usage: selftest <vectors.json>") }
    let v = loadVec(args[2])
    guard GatewayCrypto.verifyChallenge(pinned: v.gwPK, hello: v.hello, challenge: v.challenge) else { fail("golden signature rejected") }
    guard GatewayCrypto.checkBinding(challenge: v.challenge, vin: v.vin) else { fail("binding mismatch") }
    guard !GatewayCrypto.checkBinding(challenge: v.challenge, vin: v.vin + "x") else { fail("wrong vin accepted") }
    guard GatewayCrypto.info(hello: v.hello, challenge: v.challenge) == unhex(str(loadJSON(args[2]), "info")) else { fail("info mismatch") }
    var bad = v.challenge; bad[bad.startIndex + 150] ^= 1
    guard !GatewayCrypto.verifyChallenge(pinned: v.gwPK, hello: v.hello, challenge: bad) else { fail("flipped sig accepted") }
    var bad2 = v.challenge; bad2[bad2.startIndex + 5] ^= 1
    guard !GatewayCrypto.verifyChallenge(pinned: v.gwPK, hello: v.hello, challenge: bad2) else { fail("flipped body accepted") }
    guard !GatewayCrypto.verifyChallenge(pinned: v.gwPK, hello: v.hello, challenge: v.challenge.dropLast()) else { fail("short accepted") }
    print("ok")

case "seal":
    guard args.count == 4 else { fail("usage: seal <vectors.json> <out.json>") }
    let v = loadVec(args[2])
    guard GatewayCrypto.verifyChallenge(pinned: v.gwPK, hello: v.hello, challenge: v.challenge),
          GatewayCrypto.checkBinding(challenge: v.challenge, vin: v.vin) else { fail("challenge rejected") }
    do {
        let info = GatewayCrypto.info(hello: v.hello, challenge: v.challenge)
        var (enc, sender) = try GatewayCrypto.makeSender(device: v.devSK, pkE: v.pkE, info: info)
        let arm = Data([0x01]) + Data(#"{"n":"a0a1a2a3a4a5a6a7a8a9aaabacadaeaf","cap":1,"exp":123456789,"params":{}}"#.utf8)
        let msgs: [Data] = [arm, Data([0x02]), Data([0x20]) + Data("hello-camera".utf8)]
        var frames: [[String: String]] = []
        for (i, m) in msgs.enumerated() {
            let ct = try sender.seal(m, authenticating: Data())
            let f = i == 0 ? enc + ct : ct
            frames.append(["frame": hex(f), "plaintext": hex(m)])
        }
        let (key, base) = try GatewayCrypto.s2cKeys(sender: sender)
        let out: [String: Any] = ["hello": hex(v.hello), "challenge": hex(v.challenge), "frames": frames,
                                  "s2c_key": hex(key.withUnsafeBytes { Data($0) }), "s2c_base": hex(base)]
        let data = try JSONSerialization.data(withJSONObject: out, options: [.prettyPrinted, .sortedKeys])
        try data.write(to: URL(fileURLWithPath: args[3]))
        print("ok")
    } catch { fail("seal: \(error)") }

case "opens2c":
    guard args.count == 4 else { fail("usage: opens2c <go_s2c.json> <cryptokit_arm.json>") }
    // The Swift sender is random-ephemeral, so re-derive from the arm file's
    // recorded key/base, and require they equal what Go used.
    let g = loadJSON(args[2]), a = loadJSON(args[3])
    guard str(g, "key") == str(a, "s2c_key"), str(g, "base") == str(a, "s2c_base") else { fail("go key/base differ from swift") }
    let key = SymmetricKey(data: unhex(str(g, "key"))), base = unhex(str(g, "base"))
    guard let frames = g["frames"] as? [[String: String]], frames.count >= 2 else { fail("frames") }
    do {
        for (i, f) in frames.enumerated() {
            let pt = try GatewayCrypto.openS2C(key: key, base: base, counter: UInt64(i), ct: unhex(f["frame"] ?? ""))
            guard hex(pt) == f["plaintext"] else { fail("plaintext \(i) mismatch") }
        }
        // reordered: frame 1 at counter 0 must fail
        if (try? GatewayCrypto.openS2C(key: key, base: base, counter: 0, ct: unhex(frames[1]["frame"] ?? ""))) != nil { fail("reordered frame opened") }
        // bad input must throw, not crash
        if (try? GatewayCrypto.openS2C(key: key, base: base, counter: 0, ct: Data([1, 2, 3]))) != nil { fail("short ct opened") }
    } catch { fail("open: \(error)") }
    print("ok")

default: fail("unknown command")
}
