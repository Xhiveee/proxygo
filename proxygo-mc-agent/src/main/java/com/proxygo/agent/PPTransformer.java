package com.proxygo.agent;

import javassist.ByteArrayClassPath;
import javassist.ClassPool;
import javassist.CtClass;
import javassist.CtMethod;
import javassist.LoaderClassPath;

import java.lang.instrument.ClassFileTransformer;
import java.security.ProtectionDomain;

/**
 * PPTransformer - instrumenting {@link java.lang.instrument.ClassFileTransformer}.
 *
 * <p>Two strategies are applied to make the agent universal across MC cores and
 * versions:</p>
 *
 * <ol>
 *   <li><b>Netty-level (primary, universal):</b> injects
 *       {@code PPHandler.maybeHandle} at the head of Netty's
 *       {@code AbstractChannelHandlerContext.invokeChannelRead}, i.e. exactly
 *       where the raw {@link io.netty.buffer.ByteBuf} enters the pipeline BEFORE
 *       any Minecraft decoder. Netty's class/method is stable across MC
 *       versions and cores, so the PPv2 header is caught reliably.</li>
 *   <li><b>Class-level (fallback):</b> injects {@code PPHandler.handle} into the
 *       Minecraft network manager's {@code channelRead0}/{@code channelRead}
 *       when the class is matched by name.</li>
 * </ol>
 *
 * <p>Everything is defensive: if a class or method cannot be found the original
 * bytes are returned unchanged.</p>
 */
public class PPTransformer implements ClassFileTransformer {

    /** Method names that deliver an inbound frame on the MC network manager. */
    private static final String[] INBOUND_METHODS = {"channelRead0", "channelRead"};

    /** Marker used to detect an already-instrumented method body. */
    private static final String HANDLER_FQN = "com.proxygo.agent.PPHandler.handle";
    private static final String NETTY_FQN = "com.proxygo.agent.PPHandler.maybeHandle";

    /** Netty classes that carry the raw inbound ByteBuf. */
    private static final String NETTY_INVOKE = "io/netty/channel/AbstractChannelHandlerContext";

    @Override
    public byte[] transform(ClassLoader loader, String className, Class<?> classBeingRedefined,
                            ProtectionDomain protectionDomain, byte[] classfileBuffer) {
        if (className == null) {
            return null;
        }
        try {
            ClassPool pool = ClassPool.getDefault();
            String name = className.replace('/', '.'); // bytecode name is slash-form
            if (loader != null) {
                pool.insertClassPath(new LoaderClassPath(loader));
            }
            pool.insertClassPath(new ByteArrayClassPath(name, classfileBuffer));
            CtClass cc = pool.get(name);

            byte[] out = null;
            if (className.equals(NETTY_INVOKE)) {
                out = instrumentNetty(cc);
            } else if (isNetworkManager(className)) {
                out = instrument(cc);
            }
            if (out != null) {
                cc.detach();
                return out;
            }
            cc.detach();
        } catch (Throwable t) {
            PPAgent.log("transform skipped for " + className + ": " + t);
        }
        return null;
    }

    /** @return true if {@code className} looks like a Minecraft network manager. */
    private static boolean isNetworkManager(String className) {
        return className.equals("net/minecraft/network/Connection")
            || className.equals("net/minecraft/server/network/NetworkManager")
            || className.equals("net/minecraft/network/NetworkManager")
            || className.endsWith("Connection")
            || className.endsWith("NetworkManager");
    }

    /**
     * Injects the PPv2 parser at the head of Netty's static
     * {@code invokeChannelRead(AbstractChannelHandlerContext, Object)}.
     */
    private static byte[] instrumentNetty(CtClass cc) throws Exception {
        for (CtMethod m : cc.getDeclaredMethods()) {
            if (!m.getName().equals("invokeChannelRead")) {
                continue;
            }
            CtClass[] params = m.getParameterTypes();
            if (params.length == 2 && params[0].getName().contains("ChannelHandlerContext")) {
                // static: $1 = next ctx, $2 = msg (raw ByteBuf)
                m.insertBefore(NETTY_FQN + "($1, $2);");
                PPAgent.log("netty hook installed on " + cc.getName() + "#invokeChannelRead");
                return cc.toBytecode();
            }
        }
        PPAgent.log("netty hook: no invokeChannelRead(ChannelHandlerContext, Object) on " + cc.getName());
        return null;
    }

    /**
     * Injects the PPv2 parsing call into the first inbound-frame method found on
     * the Minecraft network manager.
     */
    private static byte[] instrument(CtClass cc) throws Exception {
        for (String mname : INBOUND_METHODS) {
            CtMethod m = findMethod(cc, mname);
            if (m == null) {
                continue;
            }
            if (m.isEmpty() || !m.getMethodInfo().toString().contains(HANDLER_FQN)) {
                // $0 = this, $1 = ChannelHandlerContext, $2 = inbound msg
                m.insertBefore(HANDLER_FQN + "($0, $1, $2);");
            }
            PPAgent.log("instrumented " + cc.getName() + "#" + mname);
            return cc.toBytecode();
        }
        PPAgent.log("candidate matched but NO inbound method on " + cc.getName()
            + "; tried " + String.join(",", INBOUND_METHODS)
            + ". (netty-level hook should still cover it)");
        return null;
    }

    /**
     * Finds a declared method named {@code mname} with exactly two parameters
     * whose first parameter is a ChannelHandlerContext (matched loosely).
     */
    private static CtMethod findMethod(CtClass cc, String mname) throws Exception {
        CtMethod[] methods = cc.getDeclaredMethods();
        for (CtMethod m : methods) {
            if (!mname.equals(m.getName())) {
                continue;
            }
            CtClass[] params = m.getParameterTypes();
            if (params.length == 2 && params[0].getName().contains("ChannelHandlerContext")) {
                return m;
            }
        }
        return null;
    }
}
