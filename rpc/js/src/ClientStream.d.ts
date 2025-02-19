import { grpc } from "@improbable-eng/grpc-web";
import { Response, Stream } from "./gen/proto/rpc/webrtc/v1/grpc_pb";
import { BaseStream } from "./BaseStream";
import type { ClientChannel } from "./ClientChannel";
export declare class ClientStream extends BaseStream implements grpc.Transport {
    private readonly channel;
    private headersReceived;
    private trailersReceived;
    constructor(channel: ClientChannel, stream: Stream, onDone: (id: number) => void, opts: grpc.TransportOptions);
    start(metadata: grpc.Metadata): void;
    sendMessage(msgBytes?: Uint8Array): void;
    finishSend(): void;
    private writeMessage;
    onResponse(resp: Response): void;
    private processHeaders;
    private processMessage;
    private processTrailers;
}
export declare function encodeASCII(input: string): Uint8Array;
