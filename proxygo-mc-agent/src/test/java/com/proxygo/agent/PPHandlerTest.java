package com.proxygo.agent;

import io.netty.buffer.ByteBuf;
import io.netty.buffer.Unpooled;
import org.junit.Test;

import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

/**
 * Tests for the agent: PROXY v2 parsing/substitution and the class transformer.
 */
public class PPHandlerTest {

    /** A stand-in for the Minecraft NetworkManager connection. */
    public static class FakeConn {
        private InetSocketAddress address;

        public InetSocketAddress getAddress() {
            return address;
        }
    }

    /** The transformed network manager (must end with 'NetworkManager'). */
    public static class FakeNetworkManager {
        public void channelRead0(io.netty.channel.ChannelHandlerContext ctx,
                                 io.netty.buffer.ByteBuf msg) {
            // no-op body; the agent injects PPHandler.handle here.
        }
    }

    private static byte[] buildIPv4Header(String src, int sport, String dst, int dport) {
        ByteBuf b = Unpooled.buffer();
        b.writeBytes(new byte[]{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
                0x51, 0x55, 0x49, 0x54, 0x0A});
        b.writeByte(0x21); // version 2 | PROXY
        b.writeByte(0x11); // INET | STREAM
        b.writeShort(12);  // address block length
        for (String p : src.split("\\.")) {
            b.writeByte(Integer.parseInt(p));
        }
        for (String p : dst.split("\\.")) {
            b.writeByte(Integer.parseInt(p));
        }
        b.writeShort(sport);
        b.writeShort(dport);
        int len = b.writerIndex();
        byte[] out = new byte[len];
        b.readBytes(out);
        b.release();
        return out;
    }

    @Test
    public void substitutesRealAddressAndStripsHeader() throws Exception {
        byte[] header = buildIPv4Header("203.0.113.5", 51234, "193.23.221.21", 25565);
        byte[] payload = new byte[]{0x10, 0x00, 0x51}; // fake handshake bytes
        ByteBuf buf = Unpooled.wrappedBuffer(
            Unpooled.wrappedBuffer(header), Unpooled.wrappedBuffer(payload));

        FakeConn conn = new FakeConn();
        PPHandler.handle(conn, null, buf);

        assertEquals("203.0.113.5", conn.getAddress().getAddress().getHostAddress());
        assertEquals(51234, conn.getAddress().getPort());

        // header fully consumed; the remaining readable bytes == payload
        int remaining = buf.readableBytes();
        assertEquals(payload.length, remaining);
        byte[] rest = new byte[payload.length];
        buf.readBytes(rest);
        assertArrayEquals(payload, rest);
        buf.release();
    }

    @Test
    public void handlesDirectConnectionTransparently() {
        byte[] payload = new byte[]{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
                0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10};
        ByteBuf buf = Unpooled.wrappedBuffer(payload);
        FakeConn conn = new FakeConn();
        PPHandler.handle(conn, null, buf);
        // no signature -> address untouched, buffer untouched
        assertNull(conn.getAddress());
        assertEquals(payload.length, buf.readableBytes());
        buf.release();
    }

    @Test
    public void ignoresPartialHeader() {
        ByteBuf buf = Unpooled.wrappedBuffer(new byte[]{0x0D, 0x0A, 0x0D}); // too short
        FakeConn conn = new FakeConn();
        PPHandler.handle(conn, null, buf);
        assertNull(conn.getAddress());
        buf.release();
    }

    // ----- transformer --------------------------------------------------------

    @Test
    public void transformerInstrumentsNetworkManager() throws Exception {
        // build the fake class in a PRIVATE pool so the transformer's default
        // pool does not see a frozen copy.
        javassist.ClassPool pool = new javassist.ClassPool(true);
        javassist.CtClass cc = pool.makeClass("com.proxygo.agent.FakeNetworkManager");
        cc.addMethod(javassist.CtNewMethod.make(
            "public void channelRead0(io.netty.channel.ChannelHandlerContext ctx, " +
            "io.netty.buffer.ByteBuf msg) { System.out.println(\"rcv\"); }", cc));
        byte[] original = cc.toBytecode();

        PPTransformer t = new PPTransformer();
        byte[] transformed = t.transform(Thread.currentThread().getContextClassLoader(),
            "com/proxygo/agent/FakeNetworkManager", null, null, original);

        assertNotNull(transformed);
        // the injected handler must appear in the class constant pool
        assertTrue(contains(transformed, "PPHandler"));
    }

    @Test
    public void transformerLeavesOtherClassesAlone() {
        PPTransformer t = new PPTransformer();
        byte[] sample = new byte[]{(byte) 0xCA, (byte) 0xFE, (byte) 0xBA, (byte) 0xBE};
        byte[] out = t.transform(Thread.currentThread().getContextClassLoader(),
            "com/example/Foo", null, null, sample);
        assertNull(out);
    }

    private static boolean contains(byte[] haystack, String needle) {
        byte[] n = needle.getBytes(StandardCharsets.UTF_8);
        outer:
        for (int i = 0; i + n.length <= haystack.length; i++) {
            for (int j = 0; j < n.length; j++) {
                if (haystack[i + j] != n[j]) {
                    continue outer;
                }
            }
            return true;
        }
        return false;
    }
}
