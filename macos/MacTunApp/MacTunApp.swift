import SwiftUI

@main
struct MacTunApp: App {
    @StateObject private var controller = TunController()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(controller)
                .frame(minWidth: 720, minHeight: 640)
        }
        .windowStyle(.titleBar)
    }
}
