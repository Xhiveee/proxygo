package com.proxygo.agent;

import io.netty.buffer.ByteBuf;
import io.netty.channel.ChannelHandlerContext;
import io.netty.util.AttributeKey;

import java.lang.reflect.Field;
import java.net.InetSocketAddress;
import java.net.SocketAddress;

/**
 * PPHandler - runtime logic that inspects the first inbound frame.
 *
 * <p>When a frame starts with the HAProxy PROXY v2 signature it is parsed to
 * recover the real client IP:port, that address is written back into the
 * connection's {@code address}/{@code socketAddress} field (located by
 * reflection so obfuscated or renamed fields still work), and the header bytes
 * are skipped so the engine sees only Minecraft protocol data.</p>
 *
 * <p>If no signature is present the connection is a direct link and the
 * handler is a no-op, so the agent is fully transparent.</p>
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
    private static final int VERSION_LOCAL = 2; // header version is always 2
    private static final int CMD_PROXY = 0x1;

    private static final int FAMILY_INET = 0x1;
    private static final int FAMILY_INET6 = 0x2;

    private static final AttributeKey<Boolean> DONE =
        AttributeKey.valueOf("proxygo-ppv2-handled");

    /**
     * Called from the instrumented {@code channelRead}{@code /0}.
     *
     * @param conn the network manager / connection instance (this)
     * @param ctx  the Netty pipeline context
     * @param msg  the inbound frame (a {@link ByteBuf} when data arrives)
     */
    public static void handle(Object conn, ChannelHandlerContext ctx, Object msg) {
        if (conn == null || !(msg instanceof ByteBuf)) {
            return;
        }
        ByteBuf buf = (ByteBuf) msg;

        if (ctx != null && ctx.channel().attr(DONE).get() != null) {
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

        int verCmd = buf.getUnsignedByte(idx + SIG_LEN);        // version << 4 | command
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
            // partial header - wait for the remaining bytes, do not mark done.
            return;
        }

        // Substitution is only meaningful for a PROXY (not LOCAL) command.
        if (command == CMD_PROXY && srcIp != null && srcPort > 0) {
            try {
                substitute(conn, new InetSocketAddress(srcIp, srcPort));
            } catch (RuntimeException e) {
                PPAgent.log("could not substitute address: " + e.getMessage());
            }
        }

        buf.skipBytes(headerLen);
        markDone(ctx);
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
        int b = idx + SIG_LEN + 4; // address block starts right after sig+v/c+f/p+len
        return (buf.getUnsignedByte(b)) + "." + buf.getUnsignedByte(b + 1) + "."
            + buf.getUnsignedByte(b + 2) + "." + buf.getUnsignedByte(b + 3);
    }

    private static String readIPv6(ByteBuf buf, int idx) {
        int b = idx + SIG_LEN + 4;
        byte[] raw = new byte[16];
        buf.getBytes(b, raw);
        // format as standard 8-group hex, best-effort.
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
                        // try IllegalAccess on final fields by using the field directly
                        f.set(conn, addr);
                        PPAgent.log("substituted " + c.getSimpleName() + "#" + f.getName()
                            + " -> " + addr.getAddress().getHostAddress() + ":" + addr.getPort());
                        return;
                    } catch (Throwable t) {
                        // Fall through to the next field / superclass.
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
