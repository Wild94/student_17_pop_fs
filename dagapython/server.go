package dagapython

import (
	"crypto/sha512"
	"fmt"
	"io"
	"strconv"

	"gopkg.in/dedis/crypto.v0/abstract"
	"gopkg.in/dedis/crypto.v0/ed25519"
	"gopkg.in/dedis/crypto.v0/random"
)

/*Server is used to store the server's private key and index.
All the server's methods are attached to it */
type Server struct {
	Private abstract.Scalar
	index   int
	r       abstract.Scalar //Per round secret
}

/*Commitment stores the index of the server, the commitment value and the signature for the commitment*/
type Commitment struct {
	commit abstract.Point
	Sig    ServerSignature
}

/*ServerSignature stores a signature created by a server and the server's index*/
type ServerSignature struct {
	index int
	sig   []byte
}

/*Challenge stores the collectively generated challenge and the signatures of the servers*/
type Challenge struct {
	cs   abstract.Scalar
	Sigs []ServerSignature
}

/*ServerMessage stores the message sent by a server to one or many others*/
type ServerMessage struct {
	request ClientMessage
	tags    []abstract.Point
	proofs  []ServerProof
	indexes []int
	sigs    []ServerSignature
}

/*ServerProof stores a server proof of his computations*/
type ServerProof struct {
	t1 abstract.Point
	t2 abstract.Point
	t3 abstract.Point
	c  abstract.Scalar
	r1 abstract.Scalar
	r2 abstract.Scalar
}

/*GenerateCommitment creates the commitment and its opening for the distributed challenge generation*/
func (server *Server) GenerateCommitment(context ContextEd25519) (commit *Commitment, opening abstract.Scalar, err error) {
	opening = suite.Scalar().Pick(random.Stream)
	com := suite.Point().Mul(nil, opening)
	msg, err := com.MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("Error in conversion of commit: %s", err)
	}
	sig, err := ECDSASign(server.Private, msg)
	if err != nil {
		return nil, nil, fmt.Errorf("Error in commit signature generation: %s", err)
	}
	return &Commitment{Sig: ServerSignature{index: server.index, sig: sig}, commit: com}, opening, nil
}

/*VerifyCommitmentSignature verifies that all the commitments are valid and correctly signed*/
func (server *Server) VerifyCommitmentSignature(context ContextEd25519, commits []Commitment) (err error) {
	for i, com := range commits {
		if i != com.Sig.index {
			return fmt.Errorf("Wrong index")
		}
		//TODO: How to check that a point is on the curve?

		//Covnert the commitment and verify the signature
		msg, e := com.commit.MarshalBinary()
		if e != nil {
			return fmt.Errorf("Error in conversion of commit for verification: %s", err)
		}
		err = ECDSAVerify(context.G.Y[i], msg, com.Sig.sig)
		if err != nil {
			return err
		}
	}
	return nil
}

/*CheckOpenings verifies each opening and returns the computed challenge*/
func (server *Server) CheckOpenings(context ContextEd25519, commits []Commitment, openings []abstract.Scalar) (cs abstract.Scalar, err error) {
	if len(commits) != len(openings) {
		return nil, fmt.Errorf("Lengths do not match")
	}
	cs = suite.Scalar().Zero()
	for i := 0; i < len(commits); i++ {
		c := suite.Point().Mul(nil, openings[i])
		if !commits[i].commit.Equal(c) {
			return nil, fmt.Errorf("Mismatch opening for server " + strconv.Itoa(i))
		}
		cs = suite.Scalar().Add(cs, openings[i])
	}
	return cs, nil
}

/*CheckUpdateChallenge verifies that all the previous servers computed the same challenges and that their signatures are valid
It also adds the server's signature to the list if it the round-robin is not completed (the challenge has not yet made it back to the leader)*/
func (server *Server) CheckUpdateChallenge(context ContextEd25519, cs abstract.Scalar, challenge *Challenge) (err error) {
	//Check the signatures
	msg, e := challenge.cs.MarshalBinary()
	if e != nil {
		return fmt.Errorf("Error in challenge conversion: %s", e)
	}
	for _, sig := range challenge.Sigs {
		e = ECDSAVerify(context.G.Y[sig.index], msg, sig.sig)
		if e != nil {
			return fmt.Errorf("%s", e)
		}
	}

	//Checks that the challenge values match
	if !cs.Equal(challenge.cs) {
		return fmt.Errorf("Challenge values does not match")
	}

	//Add the server's signature to the list if it is not the last one
	if len(challenge.Sigs) == len(context.G.Y) {
		return nil
	}
	sig, e := ECDSASign(server.Private, msg)
	if e != nil {
		return e
	}
	challenge.Sigs = append(challenge.Sigs, ServerSignature{index: server.index, sig: sig})

	return nil
}

/*ServerProtocol runs the server part of DAGA upon receiving a message from either a server or a client*/
func (server *Server) ServerProtocol(context ContextEd25519, msg *ServerMessage) error {
	//Step 1
	//Verify that the message is correctly formed
	if !ValidateClientMessage(msg.request) {
		return fmt.Errorf("Invalid client's request")
	}
	if len(msg.indexes) != len(msg.proofs) || len(msg.proofs) != len(msg.tags) || len(msg.tags) != len(msg.sigs) {
		return fmt.Errorf("Invalid message")
	}

	// Iteratively checks each signature if this is not the first server to receive the client's request
	data, e := msg.request.ToBytes()
	if e != nil {
		return fmt.Errorf("Error in request: %s", e)
	}
	if len(msg.indexes) != 0 {
		for i := 0; i < len(msg.indexes); i++ {
			temp, e := msg.tags[i].MarshalBinary()
			if e != nil {
				return fmt.Errorf("Error in tags: %s", e)
			}
			data = append(data, temp...)

			temp, e = msg.proofs[i].ToBytes()
			if e != nil {
				return fmt.Errorf("Error in proofs: %s", e)
			}
			data = append(data, temp...)

			data = append(data, []byte(strconv.Itoa(msg.indexes[i]))...)

			e = ECDSAVerify(context.G.Y[msg.sigs[i].index], data, msg.sigs[i].sig)
			if e != nil {
				return fmt.Errorf("Error in signature: "+strconv.Itoa(i)+"\n%s", e)
			}
		}
	}

	// Check the client proof
	if !VerifyClientProof(msg.request) {
		return fmt.Errorf("Invalid client's proof")
	}

	if len(msg.proofs) != 0 {
		for i, p := range msg.proofs {
			var valid bool
			if p.r2 == nil {
				valid = VerifyMisbehavingProof(context, i, p, msg.request.S[0])
			} else {
				valid = VerifyServerProof(context, i, msg)
			}
			if !valid {
				return fmt.Errorf("Invalid server proof")
			}
		}
	}

	//Step 2: Verify the correct behaviour of the client
	hasher := sha512.New()
	var writer io.Writer = hasher
	_, err := suite.Point().Mul(msg.request.S[0], server.Private).MarshalTo(writer)
	if err != nil {
		return fmt.Errorf("Error in shared secrets")
	}
	hash := hasher.Sum(nil)
	s := suite.Scalar().SetBytes(hash[:])
	var T abstract.Point
	var proof *ServerProof
	//Detect a misbehaving client and generate the elements of the server's message accordingly
	if !msg.request.S[server.index+1].Equal(suite.Point().Mul(msg.request.S[server.index], s)) {
		T = suite.Point().Null()
		proof, e = server.GenerateMisbehavingProof(context, msg.request.S[0])
	} else {
		inv := suite.Scalar().Inv(s)
		exp := suite.Scalar().Mul(server.r, inv)
		T = suite.Point().Mul(msg.tags[len(msg.tags)-1], exp)
		proof, e = server.GenerateServerProof(context, s, T, msg)
	}
	if e != nil {
		return e
	}

	//Signs our message
	temp, e := T.MarshalBinary()
	if e != nil {
		return fmt.Errorf("Error in T: %s", e)
	}
	data = append(data, temp...)

	temp, e = proof.ToBytes()
	if e != nil {
		return fmt.Errorf("Error in proofs: %s", e)
	}
	data = append(data, temp...)

	data = append(data, []byte(strconv.Itoa(server.index))...)

	sign, e := ECDSASign(server.Private, data)
	if e != nil {
		return fmt.Errorf("Error in own signature: %s", e)
	}

	signature := ServerSignature{sig: sign, index: server.index}

	//Step 4: Form the new message
	msg.tags = append(msg.tags, T)
	msg.proofs = append(msg.proofs, *proof)
	msg.indexes = append(msg.indexes, server.index)
	msg.sigs = append(msg.sigs, signature)

	return nil
}

/*GenerateServerProof creates the server proof for its computations*/
func (server *Server) GenerateServerProof(context ContextEd25519, s abstract.Scalar, T abstract.Point, msg *ServerMessage) (proof *ServerProof, err error) {
	//Step 1
	v1 := suite.Scalar().Pick(random.Stream)
	v2 := suite.Scalar().Pick(random.Stream)

	var a abstract.Point
	if len(msg.tags) == 0 {
		a = suite.Point().Mul(msg.request.T0, v1)
	} else {
		a = suite.Point().Mul(msg.tags[len(msg.tags)-1], v1)
	}

	exp := suite.Scalar().Neg(v2)
	b := suite.Point().Mul(T, exp)
	t1 := suite.Point().Add(a, b)

	t2 := suite.Point().Mul(nil, v1)

	t3 := suite.Point().Mul(msg.request.S[server.index+1], v2) //Accesses S[j-1]

	//Step 2
	var Tprevious abstract.Point
	if len(msg.tags) == 0 {
		Tprevious = msg.request.T0
	} else {
		Tprevious = msg.tags[len(msg.tags)-1]
	}
	//Generating the hash
	hasher := sha512.New()
	var writer io.Writer = hasher
	Tprevious.MarshalTo(writer)
	T.MarshalTo(writer)
	context.R[server.index].MarshalTo(writer)
	suite.Point().Mul(nil, suite.Scalar().One()).MarshalTo(writer)
	msg.request.S[server.index+2].MarshalTo(writer)
	msg.request.S[server.index+1].MarshalTo(writer)
	t1.MarshalTo(writer)
	t2.MarshalTo(writer)
	t3.MarshalTo(writer)
	challenge := hasher.Sum(nil)

	c := suite.Scalar().SetBytes(challenge[:])
	//Step 3
	d := suite.Scalar().Mul(c, server.Private)
	r1 := suite.Scalar().Sub(v1, d)

	e := suite.Scalar().Mul(c, s)
	r2 := suite.Scalar().Sub(v2, e)

	//Step 4
	return &ServerProof{
		t1: t1,
		t2: t2,
		t3: t3,
		c:  c,
		r1: r1,
		r2: r2,
	}, nil
}

/*VerifyServerProof verifies a server proof*/
func VerifyServerProof(context ContextEd25519, i int, msg *ServerMessage) bool {
	//Step 1
	var a abstract.Point
	if i == 0 {
		a = suite.Point().Mul(msg.request.T0, msg.proofs[i].r1)
	} else {
		a = suite.Point().Mul(msg.tags[i-1], msg.proofs[i].r1)
	}
	exp := suite.Scalar().Neg(msg.proofs[i].r2)
	b := suite.Point().Mul(msg.tags[i], exp)
	t1 := suite.Point().Add(a, b)

	d := suite.Point().Mul(nil, msg.proofs[i].r1)
	e := suite.Point().Mul(context.R[i], msg.proofs[i].c)
	t2 := suite.Point().Add(d, e)

	f := suite.Point().Mul(msg.request.S[i+1], msg.proofs[i].r2)
	g := suite.Point().Mul(msg.request.S[i+2], msg.proofs[i].c)
	t3 := suite.Point().Add(f, g)

	//Step 2
	var Tprevious abstract.Point
	if i == 0 {
		Tprevious = msg.request.T0
	} else {
		Tprevious = msg.tags[i-1]
	}
	hasher := sha512.New()
	var writer io.Writer = hasher
	Tprevious.MarshalTo(writer)
	msg.tags[i].MarshalTo(writer)
	context.R[i].MarshalTo(writer)
	suite.Point().Mul(nil, suite.Scalar().One()).MarshalTo(writer)
	msg.request.S[i+2].MarshalTo(writer)
	msg.request.S[i+1].MarshalTo(writer)
	t1.MarshalTo(writer)
	t2.MarshalTo(writer)
	t3.MarshalTo(writer)
	challenge := hasher.Sum(nil)

	c := suite.Scalar().SetBytes(challenge[:])

	if !c.Equal(msg.proofs[i].c) {
		return false
	}

	return true
}

/*GenerateMisbehavingProof creates the proof of a misbehaving client*/
func (server *Server) GenerateMisbehavingProof(context ContextEd25519, Z abstract.Point) (proof *ServerProof, err error) {
	Zs := suite.Point().Mul(Z, server.Private)

	//Step 1
	v := suite.Scalar().Pick(random.Stream)
	t1 := suite.Point().Mul(Z, v)
	t2 := suite.Point().Mul(nil, v)

	//Step 2
	hasher := sha512.New()
	var writer io.Writer = hasher
	Zs.MarshalTo(writer)
	Z.MarshalTo(writer)
	context.G.Y[server.index].MarshalTo(writer)
	suite.Point().Mul(nil, suite.Scalar().One()).MarshalTo(writer)
	t1.MarshalTo(writer)
	t2.MarshalTo(writer)
	challenge := hasher.Sum(nil)

	c := suite.Scalar().SetBytes(challenge[:])

	//Step 3
	a := suite.Scalar().Mul(c, server.Private)
	r := suite.Scalar().Sub(v, a)

	//Step 4
	return &ServerProof{
		t1: t1,
		t2: t2,
		t3: Zs,
		c:  c,
		r1: r,
		r2: nil,
	}, nil
}

/*VerifyMisbehavingProof verifies a proof of a misbehaving client*/
func VerifyMisbehavingProof(context ContextEd25519, i int, proof ServerProof, Z abstract.Point) bool {
	if proof.r2 != nil {
		return false
	}

	//Step 1
	a := suite.Point().Mul(Z, proof.r1)       //r1 = r
	b := suite.Point().Mul(proof.t3, proof.c) //t3 = Zs
	t1 := suite.Point().Add(a, b)

	d := suite.Point().Mul(nil, proof.r1) //r1 = r
	e := suite.Point().Mul(context.G.Y[i], proof.c)
	t2 := suite.Point().Add(d, e)

	//Step 2
	hasher := sha512.New()
	var writer io.Writer = hasher
	proof.t3.MarshalTo(writer)
	Z.MarshalTo(writer)
	context.G.Y[i].MarshalTo(writer)
	suite.Point().Mul(nil, suite.Scalar().One()).MarshalTo(writer)
	t1.MarshalTo(writer)
	t2.MarshalTo(writer)
	challenge := hasher.Sum(nil)

	c := suite.Scalar().SetBytes(challenge[:])

	if !c.Equal(proof.c) {
		return false
	}

	return true
}

/*GenerateNewRoundSecret creates a new secret for the server, erasing the previous one.
It returns the commitment to that secret to be included in the context*/
func (server *Server) GenerateNewRoundSecret() (R abstract.Point) {
	var C = ed25519.Curve{}
	server.r = C.Scalar().Pick(random.Stream)
	return C.Point().Mul(nil, server.r)
}

/*ToBytes is a helper function used to convert a ServerProof into []byte to be used in signatures*/
func (proof *ServerProof) ToBytes() (data []byte, err error) {
	temp, e := proof.t1.MarshalBinary()
	if e != nil {
		return nil, fmt.Errorf("Error in t1: %s", e)
	}
	data = append(data, temp...)

	temp, e = proof.t2.MarshalBinary()
	if e != nil {
		return nil, fmt.Errorf("Error in t2: %s", e)
	}
	data = append(data, temp...)

	temp, e = proof.t3.MarshalBinary()
	if e != nil {
		return nil, fmt.Errorf("Error in t3: %s", e)
	}
	data = append(data, temp...)

	temp, e = proof.c.MarshalBinary()
	if e != nil {
		return nil, fmt.Errorf("Error in c: %s", e)
	}
	data = append(data, temp...)

	temp, e = proof.r1.MarshalBinary()
	if e != nil {
		return nil, fmt.Errorf("Error in r1: %s", e)
	}
	data = append(data, temp...)

	temp, e = proof.r2.MarshalBinary()
	if e != nil {
		return nil, fmt.Errorf("Error in r2: %s", e)
	}
	data = append(data, temp...)

	return data, nil
}
