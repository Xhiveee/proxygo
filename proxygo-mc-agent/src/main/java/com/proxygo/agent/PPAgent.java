package com.proxygo.agent;

import java.lang.instrument.Instrumentation;

/**
 * PPAgent - Java instrumentation entry point.
 *
 * <p>Loaded with {@code -javaagent:proxygo-mc-agent-1.0.0.jar}, it registers a
 * {@link java.lang.instrument.ClassFileTransformer} that rewrites the
 * Minecraft network manager so that, on the very first inbound frame, it reads
 * a HAProxy PROXY v2 header and replaces the connection's remote address with
 * the real player IP.</p>
 *
 * <p>The agent works on vanilla, Paper, Spigot, Fabric, Forge and Folia and on
 * any Minecraft version 1.7.10 - 1.21+ because it never references MC classes
 * directly - everything is matched by name and reflected at runtime.</p>
 */
public final class PPAgent {

    private PPAgent() {
    }

    /**
     * Standard agent entry point invoked with {@code -javaagent}.
     *
     * @param args agent arguments (unused)
     * @param inst instrumentation facade
     */
    public static void premain(String args, Instrumentation inst) {
        if (inst == null) {
            log("Instrumentation unavailable - agent disabled");
            return;
        }
        inst.addTransformer(new PPTransformer(), true);
        log("agent loaded (PPv2 rewriter active)");
    }

    /**
     * Attach-style entry point for dynamic agents ({@code Agent-Class}).
     */
    public static void agentmain(String args, Instrumentation inst) {
        premain(args, inst);
    }

    /** Logs to stdout with a distinctive prefix. */
    public static void log(String msg) {
        System.out.println("[proxygo-agent] " + msg);
    }
}
