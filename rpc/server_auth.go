package rpc

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	rpcpb "go.viam.com/utils/proto/rpc/v1"
)

func (ss *simpleServer) authHandler(forType CredentialsType) (AuthHandler, error) {
	handler, ok := ss.authHandlers[forType]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "no auth handler for %q", forType)
	}
	return handler, nil
}

const (
	metadataFieldAuthorization     = "authorization"
	authorizationValuePrefixBearer = "Bearer "
)

// JWTClaims extends jwt.RegisteredClaims with information about the credentials as well
// as authentication metadata.
type JWTClaims struct {
	jwt.RegisteredClaims
	CredentialsType CredentialsType   `json:"rpc_creds_type,omitempty"`
	AuthMetadata    map[string]string `json:"rpc_auth_md,omitempty"`
}

// Entity entity from the claims Audience.
func (c JWTClaims) Entity() (string, error) {
	if len(c.Audience) == 0 {
		return "", status.Error(codes.Unauthenticated, "invalid claims: no audience")
	}

	return c.Audience[0], nil
}

// GetCredentialsType returns the credential type from `rpc_creds_type` claim.
func (c JWTClaims) GetCredentialsType() CredentialsType {
	return c.CredentialsType
}

// GetAuthMetadata returns the metadata from `rpc_auth_md` claim.
func (c JWTClaims) GetAuthMetadata() map[string]string {
	return c.AuthMetadata
}

// ensure JWTClaims implements Claims.
var _ Claims = JWTClaims{}

func (ss *simpleServer) Authenticate(ctx context.Context, req *rpcpb.AuthenticateRequest) (*rpcpb.AuthenticateResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, errors.New("expected metadata")
	}
	if len(md[metadataFieldAuthorization]) != 0 {
		return nil, status.Error(codes.InvalidArgument, "already authenticated; cannot re-authenticate")
	}
	forType := CredentialsType(req.Credentials.Type)
	handler, err := ss.authHandler(forType)
	if err != nil {
		return nil, err
	}
	authMD, err := handler.Authenticate(ctx, req.Entity, req.Credentials.Payload)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Errorf(codes.PermissionDenied, "failed to authenticate: %s", err.Error())
	}

	token, err := ss.signAccessTokenForEntity(forType, req.Entity, authMD)
	if err != nil {
		return nil, err
	}

	return &rpcpb.AuthenticateResponse{
		AccessToken: token,
	}, nil
}

func (ss *simpleServer) AuthenticateTo(ctx context.Context, req *rpcpb.AuthenticateToRequest) (*rpcpb.AuthenticateToResponse, error) {
	authMD, err := ss.authToHandler(ctx, req.Entity)
	if err != nil {
		return nil, err
	}

	token, err := ss.signAccessTokenForEntity(ss.authToType, req.Entity, authMD)
	if err != nil {
		return nil, err
	}

	return &rpcpb.AuthenticateToResponse{
		AccessToken: token,
	}, nil
}

func (ss *simpleServer) signAccessTokenForEntity(
	forType CredentialsType,
	entity string,
	authMD map[string]string,
) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Audience: jwt.ClaimStrings{entity},
		},
		CredentialsType: forType,
		AuthMetadata:    authMD,
		// TODO(GOUT-13): expiration
		// TODO(GOUT-12): refresh token
		// TODO(GOUT-9): more complete info
	})

	tokenString, err := token.SignedString(ss.authRSAPrivKey)
	if err != nil {
		ss.logger.Errorw("failed to sign JWT", "error", err)
		return "", status.Error(codes.PermissionDenied, "failed to authenticate")
	}

	return tokenString, nil
}

func (ss *simpleServer) authUnaryInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	if !ss.exemptMethods[info.FullMethod] {
		authEntity, err := ss.ensureAuthed(ctx)
		if err != nil {
			return nil, err
		}
		ctx = ContextWithAuthEntity(ctx, authEntity)
	}
	return handler(ctx, req)
}

func (ss *simpleServer) authStreamInterceptor(
	srv interface{},
	serverStream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	if !ss.exemptMethods[info.FullMethod] {
		authEntity, err := ss.ensureAuthed(serverStream.Context())
		if err != nil {
			return err
		}
		ctx := ContextWithAuthEntity(serverStream.Context(), authEntity)
		serverStream = ctxWrappedServerStream{serverStream, ctx}
	}
	return handler(srv, serverStream)
}

type ctxWrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (wrapped ctxWrappedServerStream) Context() context.Context {
	return wrapped.ctx
}

func tokenFromContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "authentication required")
	}
	authHeader := md.Get(metadataFieldAuthorization)
	if len(authHeader) != 1 {
		return "", status.Error(codes.Unauthenticated, "authentication required")
	}
	if !strings.HasPrefix(authHeader[0], authorizationValuePrefixBearer) {
		return "", status.Errorf(codes.Unauthenticated, "expected Authorization: %s", authorizationValuePrefixBearer)
	}
	return strings.TrimPrefix(authHeader[0], authorizationValuePrefixBearer), nil
}

var errNotTLSAuthed = errors.New("not authenticated via TLS")

func (ss *simpleServer) ensureAuthed(ctx context.Context) (interface{}, error) {
	tokenString, err := tokenFromContext(ctx)
	if err != nil {
		// check TLS state
		if ss.tlsAuthHandler == nil {
			return nil, err
		}
		var verifiedCert *x509.Certificate
		if p, ok := peer.FromContext(ctx); ok && p.AuthInfo != nil {
			if authInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok {
				verifiedChains := authInfo.State.VerifiedChains
				if len(verifiedChains) != 0 && len(verifiedChains[0]) != 0 {
					verifiedCert = verifiedChains[0][0]
				}
			}
		}
		if verifiedCert == nil {
			return nil, err
		}
		if tlsAuthEntity, tlsErr := ss.tlsAuthHandler(ctx, verifiedCert.DNSNames...); tlsErr == nil {
			return tlsAuthEntity, nil
		} else if !errors.Is(tlsErr, errNotTLSAuthed) {
			return nil, multierr.Combine(err, tlsErr)
		}
		return nil, err
	}

	var handler AuthHandler

	// Skip validating cliams until rpc_creds_type can determine if custom claim is used. Claims must be validated
	// after decoding the jwt.
	// We MUST call claims.Valid() before passing the VerifyEntity()
	jwtParser := jwt.NewParser(jwt.WithoutClaimsValidation())

	// Parse without claims and use the default provided by jwt library. This allows us to get all unknown claims.
	outToken, err := jwtParser.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Get the credential type from the claims
		credType, err := getCredentialsTypeFromMapClaims(token.Claims)
		if err != nil {
			return nil, err
		}

		handler, err = ss.authHandler(credType)
		if err != nil {
			return nil, err
		}

		if provider, ok := handler.(TokenVerificationKeyProvider); ok {
			return provider.TokenVerificationKey(token)
		}

		// signed internally
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
		}

		return &ss.authRSAPrivKey.PublicKey, nil
	})
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "unauthenticated: %s", err)
	}

	// By default use the standard rpc.JWTClaims
	var claims Claims = &JWTClaims{}

	// If AuthHandler is using CustomClaims use the claims type provided.
	if provider, ok := handler.(TokenCustomClaimProvider); ok {
		// reset the claims to the handlers version
		claims = provider.CreateClaims()
		if claims == nil {
			return nil, status.Error(codes.Internal, "invalid implementation of TokenCustomClaimProvider, cannot return nil")
		}
	}

	// For simplicity we reparse the raw JWT into the claims. The claims in outTokens.Claims are a generic map.
	// mapstructure.Decoder has issues parsing the generic map to our struct because of the RegisteredClaims struct
	// usess pointers to time.Time causing parsing issues. For now we can just reparse the json jwt token into the claim.
	_, _, err = jwtParser.ParseUnverified(outToken.Raw, claims)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "error decoding claims: %s", err)
	}

	// We MUST validate claims here. We disabled claims validation in the parser above.
	err = claims.Valid()
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "unauthenticated: %s", err)
	}

	entity, err := claims.Entity()
	if err != nil {
		return nil, err
	}

	// Pass the raw claims to the Context.
	ctx = contextWithAuthClaims(ctx, claims)

	// Pass the auth metadata to the context.
	if claims.GetAuthMetadata() != nil {
		ctx = contextWithAuthMetadata(ctx, claims.GetAuthMetadata())
	}

	return handler.VerifyEntity(ctx, entity)
}

func getCredentialsTypeFromMapClaims(in jwt.Claims) (CredentialsType, error) {
	claims, ok := in.(jwt.MapClaims)
	if !ok {
		return CredentialsType("none"), errors.New("invalid type for claims, check library implementation")
	}

	credType, found := claims["rpc_creds_type"]
	if !found {
		return CredentialsType("none"), status.Errorf(codes.Unauthenticated, "invalid claims, missing rpc_creds_type")
	}

	credTypeAsString, ok := credType.(string)
	if !ok {
		return CredentialsType("none"), status.Errorf(codes.Unauthenticated, "invalid claims, invalid rpc_creds_type")
	}

	return CredentialsType(credTypeAsString), nil
}
