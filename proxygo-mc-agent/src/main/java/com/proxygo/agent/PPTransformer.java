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
 * <p>It recognises the Minecraft network manager class (by name, since the
 * exact FQN differs per version/loader) and injects a call to
 * {@link PPHandler#handle} at the head of the inbound frame method
 * ({@code channelRead} / {@code channelRead0}). The injected call reads the
 * PROXY v2 header, substitutes the real client address and strips the header,
 * leaving the rest of the byte stream untouched for the engine.</p>
 *
 * <p>Every step is defensive: if the class or method cannot be found, or the
 * class is not the network manager, nothing is changed and {@code null} is
 * returned so the JVM keeps the original bytes.</p>
 */
public class PPTransformer implements ClassFileTransformer {

    /** Method names that deliver an inbound frame. */
    private static final String[] INBOUND_METHODS = {"channelRead0", "channelRead"};

    /** Marker used to detect an already-instrumented method body. */
    private static final String HANDLER_FQN = "com.proxygo.agent.PPHandler.handle";

    @Override
    public byte[] transform(ClassLoader loader, String className, Class<?> classBeingRedefined,
                            ProtectionDomain protectionDomain, byte[] classfileBuffer) {
        if (!isCandidate(className)) {
            return null;
        }
        try {
            ClassPool pool = ClassPool.getDefault();
            String name = className.replace('/', '.'); // bytecode name is slash-form
            if (loader != null) {
                // make sure referenced classes (Netty etc.) resolve against the same loader
                pool.insertClassPath(new LoaderClassPath(loader));
            }
            pool.insertClassPath(new ByteArrayClassPath(name, classfileBuffer));
            CtClass cc = pool.get(name);

            if (instrument(cc)) {
                byte[] out = cc.toBytecode();
                PPAgent.log("instrumented " + name + " (" + out.length + " bytes)");
                return out;
            }
            cc.detach();
        } catch (Throwable t) {
            PPAgent.log("transform skipped for " + className + ": " + t);
            // never propagate - a best-effort agent must not break the server.
        }
        return null;
    }

    /**
     * @return true if {@code className} looks like a Minecraft network manager.
     */
    private static boolean isCandidate(String className) {
        if (className == null) {
            return false;
        }
        return className.equals("net/minecraft/network/Connection")
            || className.equals("net/minecraft/server/network/NetworkManager")
            || className.equals("net/minecraft/network/NetworkManager")
            || className.endsWith("Connection")
            || className.endsWith("NetworkManager");
    }

    /**
     * Injects the PPv2 parsing call into the first inbound-frame method found.
     *
     * @param cc the network manager class
     * @return true if a method was successfully instrumented
     */
    private static boolean instrument(CtClass cc) throws Exception {
        for (String mname : INBOUND_METHODS) {
            CtMethod m = findMethod(cc, mname);
            if (m == null) {
                continue;
            }
            // Avoid double instrumentation (e.g. after a retransform).
            if (m.isEmpty() || !m.getMethodInfo().toString().contains(HANDLER_FQN)) {
                // $0 = this, $1 = ChannelHandlerContext, $2 = inbound msg
                m.insertBefore(HANDLER_FQN + "($0, $1, $2);");
            }
            return true;
        }
        PPAgent.log("candidate matched but NO incomg method (" + cc.getName()
            + "); tried " + String.join(",", INBOUND_METHODS)
            + ". Версия сервера может не совпадать с этим трансформером.");
        return false;
    }

    /**
     * Finds a declared method named {@code mname} with exactly two parameters
     * whose first parameter is a ChannelHandlerContext (matched loosely).
     */
    private static CtMethod findMethod(CtClass cc, String mname) throws Exception {
        CtMethod[] methods;
        methods = cc.getDeclaredMethods();
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
