package grpc

import (
	"bufio"
	"bytes"
	context "context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"galois/grpc/api/v2"
	"galois/pkg/lightclient"
	lcgadget "galois/pkg/lightclient/nonadjacent"
	cometbn254 "github.com/cometbft/cometbft/crypto/bn254"
	ce "github.com/cometbft/cometbft/crypto/encoding"
	"github.com/cometbft/cometbft/crypto/merkle"
	"github.com/cometbft/cometbft/libs/protoio"
	"github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	curve "github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/utils"
	backend "github.com/consensys/gnark/backend/groth16"
	backend_bn254 "github.com/consensys/gnark/backend/groth16/bn254"
	"github.com/consensys/gnark/constraint"
	cs_bn254 "github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/consensys/gnark/logger"
	gadget "github.com/consensys/gnark/std/algebra/emulated/sw_bn254"
	"github.com/rs/zerolog"
	"io"
	"log"
	"math/big"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type proverServer struct {
	grpc.UnimplementedUnionProverAPIServer
	cs      cs_bn254.R1CS
	pk      backend_bn254.ProvingKey
	vk      backend_bn254.VerifyingKey
	maxJobs uint32
	nbJobs  atomic.Uint32
	results sync.Map
}

func (*proverServer) mustEmbedUnimplementedUnionProverAPIServer() {}

func MarshalValidators(validators []*types.SimpleValidator) ([lightclient.MaxVal]lightclient.Validator, []byte, error) {
	lcValidators := [lightclient.MaxVal]lightclient.Validator{}
	// Make sure we zero initialize
	for i := 0; i < lightclient.MaxVal; i++ {
		lcValidators[i].HashableX = 0
		lcValidators[i].HashableXMSB = 0
		lcValidators[i].HashableY = 0
		lcValidators[i].HashableYMSB = 0
		lcValidators[i].Power = 0
	}
	merkleTree := make([][]byte, len(validators))
	for i, val := range validators {
		tmPK, err := ce.PubKeyFromProto(*val.PubKey)
		if err != nil {
			return lcValidators, nil, fmt.Errorf("Could not deserialize proto to tendermint public key %s", err)
		}
		var public bn254.G1Affine
		_, err = public.SetBytes(tmPK.Bytes())
		if err != nil {
			return lcValidators, nil, fmt.Errorf("Could not deserialize bn254 public key %s", err)
		}
		leaf, err := cometbn254.NewMerkleLeaf(public, val.VotingPower)
		if err != nil {
			return lcValidators, nil, fmt.Errorf("Could not create merkle leaf %s", err)
		}
		lcValidators[i].HashableX = leaf.ShiftedX
		lcValidators[i].HashableY = leaf.ShiftedY
		lcValidators[i].HashableXMSB = leaf.MsbX
		lcValidators[i].HashableYMSB = leaf.MsbY
		lcValidators[i].Power = leaf.VotingPower

		merkleTree[i], err = leaf.Hash()
		if err != nil {
			return lcValidators, nil, fmt.Errorf("Could not create merkle hash %s", err)
		}
	}
	return lcValidators, merkle.MimcHashFromByteSlices(merkleTree), nil
}

func AggregateSignatures(signatures [][]byte) (curve.G2Affine, error) {
	var aggregatedSignature curve.G2Affine
	var decompressedSignature curve.G2Affine
	for _, signature := range signatures {
		_, err := decompressedSignature.SetBytes(signature)
		if err != nil {
			return curve.G2Affine{}, fmt.Errorf("Could not decompress signature %s", err)
		}
		aggregatedSignature.Add(&aggregatedSignature, &decompressedSignature)
	}
	return aggregatedSignature, nil
}

func (p *proverServer) Poll(ctx context.Context, pollReq *grpc.PollRequest) (*grpc.PollResponse, error) {
	req := pollReq.Request

	if len(req.TrustedCommit.Validators) > lightclient.MaxVal {
		return nil, fmt.Errorf("The circuit can handle a maximum of %d validators", lightclient.MaxVal)
	}
	if len(req.UntrustedCommit.Validators) > lightclient.MaxVal {
		return nil, fmt.Errorf("The circuit can handle a maximum of %d validators", lightclient.MaxVal)
	}
	if len(req.TrustedCommit.Signatures) > len(req.TrustedCommit.Signatures) {
		return nil, fmt.Errorf("More signatures than validators")
	}
	if len(req.UntrustedCommit.Signatures) > len(req.UntrustedCommit.Signatures) {
		return nil, fmt.Errorf("More signatures than validators")
	}

	prove := func() (*grpc.ProveResponse, error) {

		log.Println("Marshalling trusted validators...")
		trustedValidators, trustedValidatorsRoot, err := MarshalValidators(req.TrustedCommit.Validators)
		if err != nil {
			return nil, fmt.Errorf("Could not marshal trusted validators %s", err)
		}

		log.Println("Aggregating trusted signature...")
		trustedAggregatedSignature, err := AggregateSignatures(req.TrustedCommit.Signatures)
		if err != nil {
			return nil, fmt.Errorf("Could not aggregate trusted signature %s", err)
		}

		log.Println("Marshalling untrusted validators...")
		untrustedValidators, untrustedValidatorsRoot, err := MarshalValidators(req.UntrustedCommit.Validators)
		if err != nil {
			return nil, fmt.Errorf("Could not marshal untrusted validators %s", err)
		}

		log.Println("Aggregating untrusted signature...")
		untrustedAggregatedSignature, err := AggregateSignatures(req.UntrustedCommit.Signatures)
		if err != nil {
			return nil, fmt.Errorf("Could not aggregate untrusted signature %s", err)
		}

		trustedInput := lcgadget.TendermintNonAdjacentLightClientInput{
			Sig:           gadget.NewG2Affine(trustedAggregatedSignature),
			Validators:    trustedValidators,
			NbOfVal:       len(req.TrustedCommit.Validators),
			NbOfSignature: len(req.TrustedCommit.Signatures),
			Bitmap:        new(big.Int).SetBytes(req.TrustedCommit.Bitmap),
		}

		untrustedInput := lcgadget.TendermintNonAdjacentLightClientInput{
			Sig:           gadget.NewG2Affine(untrustedAggregatedSignature),
			Validators:    untrustedValidators,
			NbOfVal:       len(req.UntrustedCommit.Validators),
			NbOfSignature: len(req.UntrustedCommit.Signatures),
			Bitmap:        new(big.Int).SetBytes(req.UntrustedCommit.Bitmap),
		}

		signedBytes, err := protoio.MarshalDelimited(req.Vote)
		if err != nil {
			return nil, fmt.Errorf("Could not marshal the vote %s", err)
		}

		message := cometbn254.HashToField(signedBytes)

		hashedMessage := cometbn254.HashToG2(signedBytes)

		witness := lcgadget.Circuit{
			DomainSeparationTag:      []byte(cometbn254.CometblsSigDST),
			TrustedInput:             trustedInput,
			UntrustedInput:           untrustedInput,
			ExpectedTrustedValRoot:   trustedValidatorsRoot,
			ExpectedUntrustedValRoot: untrustedValidatorsRoot,
			Message:                  message,
			HashedMessage:            gadget.NewG2Affine(hashedMessage),
		}

		privateWitness, err := frontend.NewWitness(&witness, ecc.BN254.ScalarField())
		if err != nil {
			return nil, fmt.Errorf("Could not create witness %s", err)
		}

		logger.SetOutput(os.Stdout)
		logger.Logger().Level(zerolog.TraceLevel)

		log.Println("Executing proving backend...")
		proof, err := backend.Prove(constraint.R1CS(&p.cs), backend.ProvingKey(&p.pk), privateWitness)
		if err != nil {
			return nil, fmt.Errorf("Prover failed with %s", err)
		}

		publicWitness, err := privateWitness.Public()
		if err != nil {
			return nil, fmt.Errorf("Could not extract public inputs from witness %s", err)
		}

		// F_r element
		var commitmentHash []byte
		// G1 uncompressed
		var proofCommitment []byte
		// Ugly but https://github.com/ConsenSys/gnark/issues/652
		switch _proof := proof.(type) {
		case *backend_bn254.Proof:
			if len(p.vk.PublicAndCommitmentCommitted) != 1 {
				return nil, fmt.Errorf("Expected a single proof commitment, got: %d", len(p.vk.PublicAndCommitmentCommitted))
			}
			witnesses := publicWitness.Vector().(fr.Vector)
			maxNbPublicCommitted := 0
			for _, s := range p.vk.PublicAndCommitmentCommitted {
				maxNbPublicCommitted = utils.Max(maxNbPublicCommitted, len(s))
			}
			commitmentPrehashSerialized := make([]byte, curve.SizeOfG1AffineUncompressed+maxNbPublicCommitted*fr.Bytes)
			for i := range p.vk.PublicAndCommitmentCommitted {
				copy(commitmentPrehashSerialized, _proof.Commitments[i].Marshal())
				offset := curve.SizeOfG1AffineUncompressed
				for j := range p.vk.PublicAndCommitmentCommitted[i] {
					copy(commitmentPrehashSerialized[offset:], witnesses[p.vk.PublicAndCommitmentCommitted[i][j]-1].Marshal())
					offset += fr.Bytes
				}
				if res, err := fr.Hash(commitmentPrehashSerialized[:offset], []byte(constraint.CommitmentDst), 1); err != nil {
					return nil, fmt.Errorf("Failed to hash commitment: %v", err)
				} else {
					commitmentHash = res[0].Marshal()
				}
			}
			proofCommitment = _proof.Commitments[0].Marshal()
			break
		default:
			return nil, fmt.Errorf("Impossible: proof backend must be BN254 at this point")
		}

		publicInputs, err := publicWitness.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("Could not marshal public witness %s", err)
		}

		var proofBuffer bytes.Buffer
		mem := bufio.NewWriter(&proofBuffer)
		_, err = proof.WriteRawTo(mem)
		if err != nil {
			return nil, err
		}
		mem.Flush()
		proofBz := proofBuffer.Bytes()

		var compressedProofBuffer bytes.Buffer
		mem = bufio.NewWriter(&compressedProofBuffer)
		_, err = proof.WriteTo(mem)
		if err != nil {
			return nil, err
		}
		mem.Flush()
		compressedProofBz := compressedProofBuffer.Bytes()

		// Due to how gnark proves, we not only need the ZKP A/B/C points, but also a commitment hash and proof commitment.
		// The proof is an uncompressed proof serialized by gnark, we extract A(G1)/B(G2)/C(G1) and then append the commitment hash and commitment proof from the public inputs.
		// The EVM verifier has been extended to support this two extra public inputs.
		evmProof := append(append(proofBz[:256], commitmentHash...), proofCommitment...)

		proveRes := grpc.ProveResponse{
			Proof: &grpc.ZeroKnowledgeProof{
				Content:           proofBz,
				CompressedContent: compressedProofBz,
				PublicInputs:      publicInputs,
				EvmProof:          evmProof,
			},
			TrustedValidatorSetRoot:   trustedValidatorsRoot,
			UntrustedValidatorSetRoot: untrustedValidatorsRoot,
		}

		return &proveRes, nil
	}

	reqJson, err := json.MarshalIndent(req, "", "    ")
	if err != nil {
		return nil, err
	}
	proveKey := sha256.Sum256(reqJson)

	result, found := p.results.LoadOrStore(proveKey, &grpc.ProveRequestPending{})
	if found {
		log.Println("Poll check...")

		switch _result := result.(type) {
		case *grpc.ProveRequestPending:
			return &grpc.PollResponse{
				Result: &grpc.PollResponse_Pending{
					Pending: _result,
				},
			}, nil
		case *grpc.ProveResponse:
			p.results.Delete(proveKey)
			return &grpc.PollResponse{
				Result: &grpc.PollResponse_Done{
					Done: &grpc.ProveRequestDone{
						Response: _result,
					},
				},
			}, nil
		case error:
			return &grpc.PollResponse{
				Result: &grpc.PollResponse_Failed{
					Failed: &grpc.ProveRequestFailed{
						Message: _result.Error(),
					},
				},
			}, nil
		}
	} else {
		log.Println("Poll new...")

		for true {
			nbJobs := p.nbJobs.Load()
			if nbJobs >= p.maxJobs {
				p.results.Delete(proveKey)
				return nil, fmt.Errorf("busy_building")
			} else {
				if swapped := p.nbJobs.CompareAndSwap(nbJobs, nbJobs+1); swapped {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}

		go func() {
			log.Println(string(reqJson))
			proveRes, err := prove()
			if err != nil {
				p.results.Store(proveKey, fmt.Errorf("failed to generate proof: %v", err))
			} else {
				p.results.Store(proveKey, proveRes)
				runtime.GC()
			}
			for true {
				value := p.nbJobs.Load()
				if swapped := p.nbJobs.CompareAndSwap(value, value-1); swapped {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	return &grpc.PollResponse{
		Result: &grpc.PollResponse_Pending{
			Pending: &grpc.ProveRequestPending{},
		},
	}, nil
}

func (p *proverServer) Verify(ctx context.Context, req *grpc.VerifyRequest) (*grpc.VerifyResponse, error) {
	log.Println("Verifying...")

	reqJson, err := json.MarshalIndent(req, "", "    ")
	if err != nil {
		return nil, err
	}
	log.Println(string(reqJson))

	var proof backend_bn254.Proof
	_, err = proof.ReadFrom(bytes.NewReader(req.Proof.CompressedContent))
	if err != nil {
		return nil, fmt.Errorf("Failed to read compressed proof: %w", err)
	}

	if len(req.TrustedValidatorSetRoot) != 32 {
		return nil, fmt.Errorf("The trusted validator set root must be a SHA256 hash")
	}
	if len(req.UntrustedValidatorSetRoot) != 32 {
		return nil, fmt.Errorf("The untrusted validator set root must be a SHA256 hash")
	}

	var hashedMessage fr.Element
	err = hashedMessage.SetBytesCanonical(req.HashedMessage.Value)
	if err != nil {
		return nil, fmt.Errorf("The hashed message must be a BN254 fr.Element: %w", err)
	}

	validators := [lightclient.MaxVal]lightclient.Validator{}
	for i := 0; i < lightclient.MaxVal; i++ {
		validators[i].HashableX = 0
		validators[i].HashableY = 0
		validators[i].HashableXMSB = 0
		validators[i].HashableYMSB = 0
		validators[i].Power = 0
	}

	// We don't need the private input to verify, this is present to typecheck
	dummyInput := lcgadget.TendermintNonAdjacentLightClientInput{
		Sig:           gadget.G2Affine{},
		Validators:    validators,
		NbOfVal:       0,
		NbOfSignature: 0,
		Bitmap:        0,
	}

	witness := lcgadget.Circuit{
		TrustedInput:             dummyInput,
		UntrustedInput:           dummyInput,
		ExpectedTrustedValRoot:   req.TrustedValidatorSetRoot,
		ExpectedUntrustedValRoot: req.UntrustedValidatorSetRoot,
		Message:                  hashedMessage,
	}

	privateWitness, err := frontend.NewWitness(&witness, ecc.BN254.ScalarField())
	if err != nil {
		return nil, fmt.Errorf("Unable to create private witness: %w", err)
	}

	publicWitness, err := privateWitness.Public()
	if err != nil {
		return nil, fmt.Errorf("Unable to extract public witness: %w", err)
	}

	err = backend.Verify(backend.Proof(&proof), backend.VerifyingKey(&p.vk), publicWitness)
	if err != nil {
		log.Println("Verification failed: %w", err)
		return &grpc.VerifyResponse{
			Valid: false,
		}, nil
	} else {
		return &grpc.VerifyResponse{
			Valid: true,
		}, nil
	}
}

func (p *proverServer) GenerateContract(ctx context.Context, req *grpc.GenerateContractRequest) (*grpc.GenerateContractResponse, error) {
	log.Println("Generating contract...")

	var buffer bytes.Buffer
	mem := bufio.NewWriter(&buffer)
	err := p.vk.ExportSolidity(mem)
	if err != nil {
		return nil, err
	}
	mem.Flush()

	return &grpc.GenerateContractResponse{
		Content: buffer.Bytes(),
	}, nil
}

func (p *proverServer) QueryStats(ctx context.Context, req *grpc.QueryStatsRequest) (*grpc.QueryStatsResponse, error) {
	log.Println("Querying stats...")

	return &grpc.QueryStatsResponse{
		VariableStats: &grpc.VariableStats{
			NbInternalVariables: uint32(p.cs.GetNbInternalVariables()),
			NbSecretVariables:   uint32(p.cs.GetNbSecretVariables()),
			NbPublicVariables:   uint32(p.cs.GetNbPublicVariables()),
			NbConstraints:       uint32(p.cs.GetNbConstraints()),
			NbCoefficients:      uint32(p.cs.GetNbCoefficients()),
		},
		ProvingKeyStats: &grpc.ProvingKeyStats{
			NbG1: uint32(p.pk.NbG1()),
			NbG2: uint32(p.pk.NbG2()),
		},
		VerifyingKeyStats: &grpc.VerifyingKeyStats{
			NbG1:            uint32(p.vk.NbG1()),
			NbG2:            uint32(p.vk.NbG2()),
			NbPublicWitness: uint32(p.vk.NbPublicWitness()),
		},
		// Deprecated
		CommitmentStats: &grpc.CommitmentStats{
			NbPublicCommitted:  uint32(0),
			NbPrivateCommitted: uint32(0),
		},
	}, nil
}

// Deprecated in favor of the Poll api
func (p *proverServer) Prove(ctx context.Context, req *grpc.ProveRequest) (*grpc.ProveResponse, error) {
	for true {
		pollRes, err := p.Poll(ctx, &grpc.PollRequest{
			Request: req,
		})
		if err != nil {
			return nil, fmt.Errorf("%v", err)
		}
		if done := pollRes.GetDone(); done != nil {
			return done.Response, nil
		}
		if failed := pollRes.GetFailed(); failed != nil {
			return nil, fmt.Errorf("%v", failed.Message)
		}
		time.Sleep(1 * time.Second)
	}

	panic("impossible; qed;")
}

func loadOrCreate(r1csPath string, pkPath string, vkPath string) (cs_bn254.R1CS, backend_bn254.ProvingKey, backend_bn254.VerifyingKey, error) {
	cs := cs_bn254.R1CS{}
	pk := backend_bn254.ProvingKey{}
	vk := backend_bn254.VerifyingKey{}

	if _, err := os.Stat(r1csPath); err == nil {
		if _, err = os.Stat(pkPath); err == nil {
			if _, err = os.Stat(vkPath); err == nil {
				log.Println("Loading R1CS...")
				err := readFrom(r1csPath, constraint.R1CS(&cs))
				if err != nil {
					return cs, pk, vk, err
				}

				log.Println("Loading proving key...")
				err = readFrom(pkPath, backend.ProvingKey(&pk))
				if err != nil {
					return cs, pk, vk, err
				}

				log.Println("Loading verifying key...")
				err = readFrom(vkPath, backend.VerifyingKey(&vk))
				if err != nil {
					return cs, pk, vk, err
				}

				log.Printf("VK Alpha X: %v", vk.G1.Alpha.X.String())
				log.Printf("VK Alpha Y: %v", vk.G1.Alpha.Y.String())
				log.Printf("VK Beta X0: %v", vk.G2.Beta.X.A0.String())
				log.Printf("VK Beta X1: %v", vk.G2.Beta.X.A1.String())
				log.Printf("VK Beta Y0: %v", vk.G2.Beta.Y.A0.String())
				log.Printf("VK Beta Y1: %v", vk.G2.Beta.Y.A1.String())
				log.Printf("VK Gamma X0: %v", vk.G2.Gamma.X.A0.String())
				log.Printf("VK Gamma X1: %v", vk.G2.Gamma.X.A1.String())
				log.Printf("VK Gamma Y0: %v", vk.G2.Gamma.Y.A0.String())
				log.Printf("VK Gamma Y1: %v", vk.G2.Gamma.Y.A1.String())
				log.Printf("VK Delta X0: %v", vk.G2.Delta.X.A0.String())
				log.Printf("VK Delta X1: %v", vk.G2.Delta.X.A1.String())
				log.Printf("VK Delta Y0: %v", vk.G2.Delta.Y.A0.String())
				log.Printf("VK Delta Y1: %v", vk.G2.Delta.Y.A1.String())

				return cs, pk, vk, nil
			}
		}
	}

	var circuit lcgadget.Circuit

	log.Println("Compiling circuit...")
	r1csInstance, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &circuit, frontend.WithCompressThreshold(300))
	if err != nil {
		return cs, pk, vk, err
	}

	cs = *r1csInstance.(*cs_bn254.R1CS)

	log.Println("Setup PK/VK")
	err = backend_bn254.Setup(&cs, &pk, &vk)
	if err != nil {
		return cs, pk, vk, err
	}

	err = saveTo(r1csPath, r1csInstance)
	if err != nil {
		return cs, pk, vk, err
	}
	err = saveTo(pkPath, backend.ProvingKey(&pk))
	if err != nil {
		return cs, pk, vk, err
	}
	err = saveTo(vkPath, backend.VerifyingKey(&vk))
	if err != nil {
		return cs, pk, vk, err
	}

	return cs, pk, vk, nil
}

func NewProverServer(maxJobs uint32, r1csPath string, pkPath string, vkPath string) (*proverServer, error) {
	cs, pk, vk, err := loadOrCreate(r1csPath, pkPath, vkPath)
	if err != nil {
		return nil, err
	}

	runtime.GC()

	return &proverServer{cs: cs, pk: pk, vk: vk, maxJobs: maxJobs}, nil
}

func readFrom(file string, obj io.ReaderFrom) error {
	f, err := os.OpenFile(file, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	obj.ReadFrom(f)
	return nil
}

func saveTo(file string, x io.WriterTo) error {
	log.Printf("Saving %s\n", file)
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	written, err := x.WriteTo(w)
	if err != nil {
		return err
	}
	log.Printf("Saved %d bytes\n", written)
	w.Flush()
	return nil
}