# Aqara Multi-State Sensor P100 slide-in holder (approximate, parametric)
# Official P100 size: 45 x 31 x 13 mm
import manifold3d as m
import numpy as np, trimesh

# ---- parameters (mm) ----
DEV_L, DEV_W, DEV_H = 45.0, 31.0, 13.0
CLR      = 0.4        # clearance per side
BASE_T   = 3.0        # back plate thickness
WALL     = 2.0        # side rail thickness
LIP      = 1.5        # lip overhang that holds the sensor
LIP_T    = 1.2        # lip thickness
STOP_H   = 5.0        # bottom stop height above plate
CORNER_R = 4.0
HOLE_D, CSK_D = 4.2, 8.5   # screw hole / countersink

in_w = DEV_W + 2*CLR
in_l = DEV_L + CLR
in_h = DEV_H + CLR
out_w = in_w + 2*WALL
out_l = in_l + WALL          # stop wall at bottom end, top end open

def rrect(w, l, r, h):
    sq = m.CrossSection.square([w - 2*r, l - 2*r], center=True)
    return sq.offset(r, m.JoinType.Round, circular_segments=48).extrude(h)

def box(w, l, h, x=0, y=0, z=0):
    return m.Manifold.cube([w, l, h], center=False).translate([x - w/2, y - l/2, z])

# back plate
body = rrect(out_w, out_l, CORNER_R, BASE_T)

# side rails with lips (C-channel)
rail_h = in_h + LIP_T
for s in (-1, 1):
    x = s * (in_w/2 + WALL/2)
    body += box(WALL, out_l, BASE_T + rail_h, x, 0, 0)
    # lip, with 45deg chamfer underneath so it prints without supports
    lip_x = s * (in_w/2 - LIP/2)
    body += box(LIP, out_l, LIP_T, lip_x, 0, BASE_T + in_h)
    pts = [(0, 0), (-s*LIP, 0), (0, -LIP)]
    if s < 0: pts = pts[::-1]          # keep CCW winding
    tri = m.CrossSection([pts])
    ch = tri.extrude(out_l).rotate([90, 0, 0]).translate([s*in_w/2, out_l/2, BASE_T + in_h])
    body += ch

# bottom stop (y = -)
body += box(in_w, WALL, STOP_H,
            0, -out_l/2 + WALL/2, BASE_T)

# thumb notch in stop so the sensor can be pushed out
body -= box(12, WALL + 2, STOP_H + 1, 0, -out_l/2 + WALL/2, BASE_T + 2.5)

# centre screw hole with countersink (head sits below device)
cy = WALL/2
body -= m.Manifold.cylinder(BASE_T + 2, HOLE_D/2, HOLE_D/2, 48).translate([0, cy, -1])
body -= m.Manifold.cylinder(2.2, HOLE_D/2, CSK_D/2, 48).translate([0, cy, BASE_T - 2.2 + 0.001])

mesh = body.to_mesh()
t = trimesh.Trimesh(mesh.vert_properties[:, :3], mesh.tri_verts)
print("watertight:", t.is_watertight, "size:", np.round(t.extents, 2))
t.export("/home/user/webrtc/p100_mount/aqara_p100_mount.stl")
