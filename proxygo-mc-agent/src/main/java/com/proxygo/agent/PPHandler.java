package com.proxygo.agent;

import io.netty.buffer.ByteBuf;
import io.netty.channel.ChannelHandler;
import io.netty.channel.ChannelHandlerContext;
import io.netty.channel.ChannelPipeline;
import io.netty.util.AttributeKey;

import java.lang.reflect.Field;
import java.net.InetSocketAddress;
import java.net.SocketAddress;
import java.util.Map;

/**
 * PPHandler - runtime logic that inspects the first inbound frame.
 *
 * <p>Two entry points are injected:
 * <ul>
 *   <li>{@link #handle} - from the Minecraft network manager class (conn is known
 *       directly).</li>
 *   <li>{@link #maybeHandle} - from Netty's {@code AbstractChannelHandlerContext
 *       .invokeChannelRead}, which catches the raw {@link ByteBuf} BEFORE any
 *       Minecraft decoder. This makes the agent universal across cores and
 *       versions (Vanilla/Paper/Spigot/Fabric/Forge/Folia), because the Netty
 *       class is stable.</li>
 * </ul>
 *
 * <p>When a frame starts with the HAProxy PROXY v2 signature, it is parsed, the
 * real client address is written back into the connection's
 * {@code address}/{@code socketAddress} field (located by type or by scanning
 * the pipeline), and the header bytes are skipped so the engine sees only
 * Minecraft protocol data. Without a signature the handler is a transparent
 * no-op.</p>
 */
public final class PPHandler {

    private PPHandler() {
    }

    /** PROXY v2 signature (12 bytes). */
    private static final byte[] SIG = new byte[]{
        0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
        0x51, 0x55, 0x49, 0x54, 0x0A
    };

    private static final int SIG_LEN = 12;
    private static final int VERSION_LOCAL = 2;
    private static final int CMD_PROXY = 0x1;

    private static final int FAMILY_INET = 0x1;
    private static final int FAMILY_INET6 = 0x2;

    private static final AttributeKey<Boolean> DONE =
        AttributeKey.valueOf("proxygo-ppv2-handled");

    /** Entry point injected into the Minecraft network manager. */
    public static void handle(Object conn, ChannelHandlerContext ctx, Object msg) {
        process(msg, ctx, conn);
    }

    /**
     * Entry point injected into Netty's {@code AbstractChannelHandlerContext
     * .invokeChannelRead}. The connection object is located by scanning the
     * pipeline, so no Minecraft-specific class is needed.
     */
    public static void maybeHandle(Object ctxObj, Object msg) {
        if (!(ctxObj instanceof ChannelHandlerContext)) {
            return;
        }
        process(msg, (ChannelHandlerContext) ctxObj, null);
    }

    private static void process(Object msg, ChannelHandlerContext ctx, Object explicitConn) {
        if (!(msg instanceof ByteBuf) || ctx == null) {
            return;
        }
        ByteBuf buf = (ByteBuf) msg;

        if (ctx.channel().attr(DONE).get() != null) {
            return;
        }
        // Not enough bytes to inspect yet; wait for the next segment.
        if (!buf.isReadable(SIG_LEN + 4)) {
            return;
        }
        int idx = buf.readerIndex();
        if (!hasSignature(buf, idx)) {
            markDone(ctx);
            return; // direct connection - transparent pass-through
        }

        int verCmd = buf.getUnsignedByte(idx + SIG_LEN);
        int version = (verCmd >> 4) & 0x0F;
        int command = verCmd & 0x0F;
        if (version != VERSION_LOCAL) {
            markDone(ctx);
            return;
        }
        int famProto = buf.getUnsignedByte(idx + SIG_LEN + 1);
        int family = (famProto >> 4) & 0x0F;
        int addrLen = buf.getUnsignedShort(idx + SIG_LEN + 2);

        String srcIp;
        int srcPort;
        int headerLen;
        switch (family) {
            case FAMILY_INET:
                if (addrLen < 12) {
                    markDone(ctx);
                    return;
                }
                srcIp = readIPv4(buf, idx);
                srcPort = buf.getUnsignedShort(idx + SIG_LEN + 4 + 8);
                headerLen = SIG_LEN + 4 + 12;
                break;
            case FAMILY_INET6:
                if (addrLen < 36) {
                    markDone(ctx);
                    return;
                }
                srcIp = readIPv6(buf, idx);
                srcPort = buf.getUnsignedShort(idx + SIG_LEN + 4 + 32);
                headerLen = SIG_LEN + 4 + 36;
                break;
            default:
                srcIp = null;
                srcPort = 0;
                headerLen = SIG_LEN + 4 + addrLen;
        }

        if (!buf.isReadable(headerLen)) {
            return; // partial header - wait for the remaining bytes
        }

        if (command == CMD_PROXY && srcIp != null && srcPort > 0) {
            Object target = explicitConn != null ? explicitConn : findConn(ctx);
            if (target != null) {
                try {
                    substitute(target, new InetSocketAddress(srcIp, srcPort));
                } catch (RuntimeException e) {
                    PPAgent.log("could not substitute address: " + e.getMessage());
                }
            } else {
                PPAgent.log("no Connection/NetworkManager found in pipeline for substitution");
            }
        }

        buf.skipBytes(headerLen);
        markDone(ctx);
        PPAgent.log("PPv2 header consumed and stripped (" + headerLen + " bytes)");
    }

    // ----- signature / address reading -------------------------------------

    private static boolean hasSignature(ByteBuf buf, int idx) {
        for (int i = 0; i < SIG_LEN; i++) {
            if (buf.getByte(idx + i) != SIG[i]) {
                return false;
            }
        }
        return true;
    }

    private static String readIPv4(ByteBuf buf, int idx) {
        int b = idx + SIG_LEN + 4;
        return (buf.getUnsignedByte(b)) + "." + buf.getUnsignedByte(b + 1) + "."
            + buf.getUnsignedByte(b + 2) + "." + buf.getUnsignedByte(b + 3);
    }

    private static String readIPv6(ByteBuf buf, int idx) {
        int b = idx + SIG_LEN + 4;
        byte[] raw = new byte[16];
        buf.getBytes(b, raw);
        StringBuilder sb = new StringBuilder(39);
        for (int i = 0; i < 16; i += 2) {
            if (i > 0) {
                sb.append(':');
            }
            sb.append(String.format("%02x%02x", raw[i] & 0xFF, raw[i + 1] & 0xFF));
        }
        return sb.toString();
    }

    private static void markDone(ChannelHandlerContext ctx) {
        if (ctx != null) {
            ctx.channel().attr(DONE).set(Boolean.TRUE);
        }
    }

    // ----- locating the connection object -----------------------------------

    private static Object findConn(ChannelHandlerContext ctx) {
        try {
            ChannelPipeline pipe = ctx.pipeline();
            for (Map.Entry<String, ChannelHandler> e : pipe.toMap().entrySet()) {
                Object h = e.getValue();
                if (h == null) {
                    continue;
                }
                String n = h.getClass().getName();
                if (n.contains("Connection") || n.contains("NetworkManager")) {
                    return h;
                }
            }
        } catch (Throwable t) {
            // ignore - best-effort
        }
        return null;
    }

    // ----- reflection substitution ------------------------------------------

    /**
     * Locates the InetSocketAddress/SocketAddress field on the connection (or
     * any superclass) and writes the real address into it. The field is found
     * by type so obfuscated names are irrelevant.
     */
    private static void substitute(Object conn, InetSocketAddress addr) {
        Class<?> c = conn.getClass();
        while (c != null && c != Object.class) {
            for (Field f : c.getDeclaredFields()) {
                if (isAddressField(f)) {
                    try {
                        f.setAccessible(true);
                        f.set(conn, addr);
                        PPAgent.log("substituted " + c.getSimpleName() + "#" + f.getName()
                            + " -> " + addr.getAddress().getHostAddress() + ":" + addr.getPort());
                        return;
                    } catch (Throwable t) {
                        // fall through to the next field / superclass
                    }
                }
            }
            c = c.getSuperclass();
        }
        PPAgent.log("no substitutable address field on " + conn.getClass().getName());
    }

    private static boolean isAddressField(Field f) {
        Class<?> t = f.getType();
        return InetSocketAddress.class.isAssignableFrom(t)
            || SocketAddress.class.isAssignableFrom(t);
    }
}
