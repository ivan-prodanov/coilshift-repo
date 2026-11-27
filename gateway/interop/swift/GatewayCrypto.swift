// GatewayCrypto.swift - phone-side crypto for the airgapp gateway.
// Foundation + CryptoKit only (iOS 17+). This exact file is copied into the app.
import Foundation
import CryptoKit

enum GatewayCryptoError: Error {
    case badInput
}

@available(iOS 17.0, macOS 14.0, *)
enum GatewayCrypto {
    static let suite = HPKE.Ciphersuite.P256_SHA256_AES_GCM_256
    static let challengeLen = 185
    static let challengeBodyLen = 121

    static func sha256(_ parts: Data...) -> Data {
        var h = SHA256()
        for p in parts { h.update(data: p) }
        return Data(h.finalize())
    }

    /// binding = SHA-256("airgapp-gw/1 car" || VIN)
    static func binding(vin: String) -> Data {
        sha256(Data("airgapp-gw/1 car".utf8), Data(vin.utf8))
    }

    /// The phone MUST check this before sealing anything to the car.
    static func checkBinding(challenge: Data, vin: String) -> Bool {
        guard challenge.count == challengeLen else { return false }
        let c = Data(challenge)  // normalise indices
        return c[89..<121] == binding(vin: vin)
    }

    /// Verifies the gateway signature (raw r||s over the challenge pre-image).
    /// CryptoKit hashes the data with SHA-256 itself, which equals the Go digest
    /// SHA-256("airgapp-gw/1 challenge" || hello || challenge-without-sig).
    static func verifyChallenge(pinned: P256.Signing.PublicKey, hello: Data, challenge: Data) -> Bool {
        guard challenge.count == challengeLen else { return false }
        let c = Data(challenge)
        guard let sig = try? P256.Signing.ECDSASignature(rawRepresentation: c[challengeBodyLen...]) else {
            return false
        }
        var msg = Data("airgapp-gw/1 challenge".utf8)
        msg.append(hello)
        msg.append(c[..<challengeBodyLen])
        return pinned.isValidSignature(sig, for: msg)
    }

    /// info = "airgapp-gw/1" || 0x00 || SHA-256(hello || challenge)
    static func info(hello: Data, challenge: Data) -> Data {
        var d = Data("airgapp-gw/1".utf8)
        d.append(0)
        d.append(sha256(hello, challenge))
        return d
    }

    /// HPKE Auth mode sender, authenticated by the device key. enc is 65 bytes.
    static func makeSender(device: P256.KeyAgreement.PrivateKey,
                           pkE: P256.KeyAgreement.PublicKey,
                           info: Data) throws -> (enc: Data, sender: HPKE.Sender) {
        let s = try HPKE.Sender(recipientKey: pkE, ciphersuite: suite, info: info, authenticatedBy: device)
        let enc = Data(s.encapsulatedKey)
        guard enc.count == 65 else { throw GatewayCryptoError.badInput }
        return (enc, s)
    }

    /// Car->phone key and base nonce from the HPKE exporter.
    static func s2cKeys(sender: HPKE.Sender) throws -> (key: SymmetricKey, base: Data) {
        let key = try sender.exportSecret(context: Data("airgapp-gw/1 s2c key".utf8), outputByteCount: 32)
        let baseKey = try sender.exportSecret(context: Data("airgapp-gw/1 s2c nonce".utf8), outputByteCount: 12)
        return (key, baseKey.withUnsafeBytes { Data($0) })
    }

    /// Opens one car->phone frame. The CALLER owns the counter (starts at 0,
    /// +1 per successfully opened frame) and MUST treat any thrown error as
    /// fatal for the session: never skip, retry or reorder frames.
    static func openS2C(key: SymmetricKey, base: Data, counter: UInt64, ct: Data) throws -> Data {
        guard base.count == 12 else { throw GatewayCryptoError.badInput }
        var nonce = [UInt8](base)
        for i in 0..<8 {
            nonce[4 + i] ^= UInt8((counter >> UInt64(8 * (7 - i))) & 0xff)
        }
        let box = try AES.GCM.SealedBox(combined: Data(nonce) + Data(ct))
        return try AES.GCM.open(box, using: key, authenticating: Data())
    }
}
